package main

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar"
	"github.com/bradenaw/juniper/xsync"
	"github.com/dgraph-io/ristretto"
	"github.com/klauspost/compress/zstd"
	pb "github.com/rinsuki/mayakashi/proto"
	"github.com/winfsp/cgofuse/fuse"
	"google.golang.org/protobuf/proto"
)

const INDEX_MAGIC = "MARI"

type FileInfo struct {
	MarEntry    *pb.FileEntry
	ZipEntry    *zip.File
	ArchiveFile string
}

type DirInfo struct {
	Files       map[string]string
	Directories map[string]string
}

type ChunkCache struct {
	ChunkNo int
	Data    []byte
}

type SharedFileHandler struct {
	File  *os.File
	Mutex sync.Mutex
}

type RenameRequest struct {
	OldPath string
	NewPath string
}

type MayakashiFS struct {
	fuse.FileSystemBase
	Directories          map[string]*DirInfo
	Files                map[string]FileInfo
	ArchivePrefix        string
	Count                uint64
	ChunkCache           *ristretto.Cache
	OverlayDir           string
	OverlayCount         uint64
	OverlayFileHandlers  xsync.Map[uint64, *SharedFileHandler]
	RemoveRequestedPaths xsync.Map[string, string]
	RenameRequestedPaths xsync.Map[string, RenameRequest]
	ReadonlyPrefixes     []string
	SlowReadLog          *os.File
	LastDatRead          time.Time
	ZipCache             map[string]*xsync.Pool[*zip.ReadCloser]
	PreloadGlobs         []string
	PProfAddr            string
}

func recoverHandler() {
	if r := recover(); r != nil {
		fmt.Println("Recovered in f", r)
		for depth := 0; ; depth++ {
			_, file, line, ok := runtime.Caller(depth)
			if !ok {
				break
			}
			log.Printf("======> %d: %v:%d", depth, file, line)
		}
		time.Sleep(1 * time.Second)
		panic(r)
	}
}

func NewMayakashiFS() *MayakashiFS {
	// sf, err := os.Create("slowread.log")
	// if err != nil {
	// 	panic(err)
	// }
	cache, err := ristretto.NewCache(&ristretto.Config{
		MaxCost:     4 * 1024 * 1024 * 1024, // 4GiB
		NumCounters: 1024 * 1024 * 10,       // 10MiB * 3
		BufferItems: 64,
	})

	if err != nil {
		panic(err)
	}

	return &MayakashiFS{
		Files:                map[string]FileInfo{},
		Directories:          map[string]*DirInfo{},
		ChunkCache:           cache,
		OverlayCount:         0x1000_0000,
		OverlayFileHandlers:  xsync.Map[uint64, *SharedFileHandler]{},
		RemoveRequestedPaths: xsync.Map[string, string]{},
		ZipCache:             map[string]*xsync.Pool[*zip.ReadCloser]{},
		// SlowReadLog:          sf,
	}
}

func (fs *MayakashiFS) ParseFile(file string) error {
	var options ArchiveReadOptions

	if file == "" || strings.HasPrefix(file, "# ") {
		// ignore empty or starts with "# "
		return nil
	}

	for {
		shouldBreak := true

		if strings.HasPrefix(file, "addprefix=") {
			af := strings.SplitN(file, ":", 2)
			ap := af[0]
			file = af[1]
			ap = strings.SplitN(ap, "=", 2)[1]
			if len(ap) > 0 && !strings.HasPrefix(ap, "/") {
				ap = "/" + ap
			}
			for strings.HasSuffix(ap, "/") {
				ap = ap[:len(ap)-1]
			}
			if options.AdditionalPrefix != "" {
				return fmt.Errorf("additional prefix already set (%s)", options.AdditionalPrefix)
			}
			options.AdditionalPrefix = ap
			shouldBreak = false
		}

		if strings.HasPrefix(file, "stripprefix=") {
			sf := strings.SplitN(file, ":", 2)
			file = sf[1]
			sf = strings.SplitN(sf[0], "=", 2)
			sp := sf[1]
			if len(sp) > 0 && !strings.HasPrefix(sp, "/") {
				sp = "/" + sp
			}
			if options.StripPrefix != "" {
				return fmt.Errorf("strip prefix already set (%s)", options.StripPrefix)
			}
			options.StripPrefix = sp
			shouldBreak = false
		}

		if strings.HasPrefix(file, "roprefix=") {
			rop := strings.SplitN(file, "=", 2)
			file = rop[1]
			if !strings.HasPrefix(file, "/") {
				file = "/" + file
			}
			fs.ReadonlyPrefixes = append(fs.ReadonlyPrefixes, file)
			return nil
		}

		if strings.HasPrefix(file, "overlaydir=") {
			od := strings.SplitN(file, "=", 2)
			file = od[1]
			fs.OverlayDir = file
			return nil
		}

		if strings.HasPrefix(file, "preload=") {
			od := strings.SplitN(file, "=", 2)
			file = od[1]
			fs.PreloadGlobs = append(fs.PreloadGlobs, file)
			return nil
		}

		if strings.HasPrefix(file, "pprof=") {
			od := strings.SplitN(file, "=", 2)
			file = od[1]
			fs.PProfAddr = file
			return nil
		}

		for strings.HasPrefix(file, "onlyglob=") {
			oa := strings.SplitN(file, ":", 2)
			file = oa[1]
			options.IncludedGlobs = append(options.IncludedGlobs, oa[0][len("onlyglob="):])
			shouldBreak = false
		}

		if strings.HasPrefix(file, "ziplocale=") {
			zf := strings.SplitN(file, ":", 2)
			file = zf[1]
			zf = strings.SplitN(zf[0], "=", 2)
			locale := zf[1]
			if err := options.SetZipLocale(locale); err != nil {
				return err
			}
			shouldBreak = false
		}

		if strings.HasPrefix(file, "commandsfile=") {
			// commands are splitted by line.

			cf := strings.SplitN(file, "=", 2)
			file = cf[1]

			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()

			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := scanner.Text()
				fmt.Println("Loading from file", file, "Command: "+line)
				if err := fs.ParseFile(line); err != nil {
					return err
				}
			}
			return nil
		}

		if shouldBreak {
			break
		}
	}

	if strings.HasSuffix(file, ".zip") {
		return fs.parseZipFile(file, options)
	}

	if strings.HasSuffix(file, ".mar") {
		return fs.parseMARFile(file, options)
	}

	return fmt.Errorf("unknown file type (filename suffix): %s", file)
}

func (fs *MayakashiFS) getZipReadCloser(file string) *zip.ReadCloser {
	pool, ok := fs.ZipCache[file]
	if !ok {
		p := xsync.NewPool[*zip.ReadCloser](func() *zip.ReadCloser {
			zf, err := zip.OpenReader(file)
			if err != nil {
				panic(err)
			}
			return zf
		})
		pool = &p
		fs.ZipCache[file] = pool
	}
	return pool.Get()
}

func (fs *MayakashiFS) putZipReadCloser(file string, zf *zip.ReadCloser) {
	pool, ok := fs.ZipCache[file]
	if !ok {
		panic("cache not found")
	}
	pool.Put(zf)
}

func (fs *MayakashiFS) parseZipFile(file string, o ArchiveReadOptions) error {
	zf := fs.getZipReadCloser(file)
	defer fs.putZipReadCloser(file, zf)

	var fileCount int

	for _, f := range zf.File {
		if f.NonUTF8 {
			f.Name = o.ConvertZipFileName(f.Name)
		}
		origPath := o.GetFilePath(f.Name)
		if origPath == "" {
			continue
		}

		lowerPath := strings.ToLower(origPath)
		fs.Files[lowerPath] = FileInfo{
			MarEntry:    nil,
			ZipEntry:    f,
			ArchiveFile: file,
		}

		dir := origPath[:strings.LastIndex(origPath, "/")]
		if f.FileInfo().IsDir() {
			// just create directory
			fs.getDirInfo(dir)
		} else {
			fs.Directories[fs.getDirInfo(dir)].Files[strings.ToLower(origPath)] = origPath
			fileCount += 1
		}
	}
	fmt.Printf("Loaded %d files\n", fileCount)

	return nil
}

func (fs *MayakashiFS) parseMARFile(file string, o ArchiveReadOptions) error {

	f, err := os.Open(file + ".idx")
	if err != nil {
		return err
	}
	defer f.Close()
	// read magic
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return err
	}

	if string(magic) != INDEX_MAGIC {
		panic("invalid magic")
	}

	// read compressed length
	var compressedLength uint32
	if err = binary.Read(f, binary.BigEndian, &compressedLength); err != nil {
		return err
	}

	// read decompressed length
	var decompressedLength uint32
	if err = binary.Read(f, binary.BigEndian, &decompressedLength); err != nil {
		return err
	}

	// read data
	data := make([]byte, compressedLength)
	if _, err := io.ReadFull(f, data); err != nil {
		return err
	}

	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return err
	}

	data, err = decoder.DecodeAll(data, make([]byte, 0, int(decompressedLength)))
	if err != nil {
		return err
	}

	var indexFile pb.FileIndexFile
	if err := proto.Unmarshal(data, &indexFile); err != nil {
		return err
	}

	fileCount := 0

	for _, entry := range indexFile.Entries {
		origPath := o.GetFilePath(entry.Info.Path)
		if origPath == "" {
			continue
		}

		lowerPath := strings.ToLower(origPath)
		fs.Files[lowerPath] = FileInfo{
			MarEntry:    entry,
			ArchiveFile: file,
		}

		dir := origPath[:strings.LastIndex(origPath, "/")]
		fs.Directories[fs.getDirInfo(dir)].Files[strings.ToLower(origPath)] = origPath
		fileCount += 1
	}
	fmt.Printf("Loaded %d files\n", fileCount)

	return nil
}

func (fs *MayakashiFS) getDirInfo(dirPath string) string {
	if dirPath == "" {
		dirPath = "/"
	}
	lowerDirPath := strings.ToLower(dirPath)
	dirInfo, ok := fs.Directories[lowerDirPath]
	if !ok {
		dirInfo = &DirInfo{
			Files:       map[string]string{},
			Directories: map[string]string{},
		}
		fs.Directories[lowerDirPath] = dirInfo
		upDir := dirPath[:strings.LastIndex(dirPath, "/")]
		if upDir == "" {
			upDir = "/"
		}
		if upDir != dirPath {
			fs.Directories[fs.getDirInfo(upDir)].Directories[strings.ToLower(dirPath)] = dirPath
		}
	}
	return lowerDirPath
}

func (fs *MayakashiFS) getOverlayPath(path string) *string {
	if fs.OverlayDir == "" {
		return nil
	}
	for _, prefix := range fs.ReadonlyPrefixes {
		if strings.HasPrefix(strings.ToLower(path), strings.ToLower(prefix)) {
			return nil
		}
	}

	overlayPath := fs.OverlayDir + path
	return &overlayPath
}

func GetFuseStatFromMarEntry(e *pb.FileEntry, stat *fuse.Stat_t) {
	var size int64
	for _, chunk := range e.Info.Chunks {
		size += int64(chunk.OriginalLength)
	}
	stat.Mode = fuse.S_IFREG | 0777
	stat.Size = size
	time := fuse.NewTimespec(e.Info.ModifiedTime.AsTime())
	stat.Ctim = time
	stat.Mtim = time
	stat.Blocks = 1
}
func GetFuseStatFromZipEntry(e *zip.File, stat *fuse.Stat_t) {
	info := e.FileInfo()
	stat.Mode = fuse.S_IFREG | 0777
	stat.Size = info.Size()
	time := fuse.NewTimespec(info.ModTime())
	stat.Ctim = time
	stat.Mtim = time
	stat.Blocks = 1
}
func GetFuseStatFromFileInfo(fi *FileInfo, stat *fuse.Stat_t) {
	if fi.MarEntry != nil {
		GetFuseStatFromMarEntry(fi.MarEntry, stat)
	} else {
		GetFuseStatFromZipEntry(fi.ZipEntry, stat)
	}
}
func (fi *FileInfo) GetFilename() string {
	var path string
	if fi.MarEntry != nil {
		path = fi.MarEntry.Info.Path
	} else {
		path = fi.ZipEntry.Name
	}
	return path[strings.LastIndex(path, "/")+1:]
}
func (fs *MayakashiFS) Statfs(path string, stat *fuse.Statfs_t) int {
	stat.Bfree = 0x_1000_0000
	stat.Bavail = 0x_1000_0000
	stat.Blocks = 0x_1000_0000
	stat.Bsize = 1
	stat.Frsize = 4096
	return 0
}

func (fs *MayakashiFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	defer recoverHandler()
	if path == "/" {
		stat.Mode = fuse.S_IFDIR | 0777
		return 0
	}

	if strings.Contains(path, "/UnityCrashHandler64.exe") {
		return -fuse.ENOENT
	}

	overlayPath := fs.getOverlayPath(path)
	if overlayPath != nil {
		if us, err := os.Stat(*overlayPath); err == nil {
			if us.IsDir() {
				stat.Mode = fuse.S_IFDIR | 0777
			} else {
				stat.Mode = fuse.S_IFREG | 0777
				stat.Size = us.Size()
			}
			stat.Ctim = fuse.NewTimespec(us.ModTime())
			stat.Mtim = fuse.NewTimespec(us.ModTime())
			return 0
		} else {
			// println("failed to stat", overlayPath, err)
		}
	}

	// fmt.Println("getattr", path)

	if file, ok := fs.Files[strings.ToLower(path)]; ok {
		GetFuseStatFromFileInfo(&file, stat)
		return 0
	}

	dir := fs.Directories[strings.ToLower(path)]

	if dir != nil {
		stat.Mode = fuse.S_IFDIR | 0777
		return 0
	}

	if !strings.Contains(path, "/._") && overlayPath != nil {
		// fmt.Println("getattr but failed", path)
	}
	return -fuse.ENOENT
}

func (fs *MayakashiFS) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {
	defer recoverHandler()
	println("listing", path)
	fill(".", nil, 0)
	fill("..", nil, 0)

	filenames := map[string]struct{}{}
	filenames["unitycrashhandler64.exe"] = struct{}{}
	haveSomeFilesInOverlay := false

	if overlayPath := fs.getOverlayPath(path); overlayPath != nil {
		files, err := ioutil.ReadDir(*overlayPath)
		if err == nil {
			haveSomeFilesInOverlay = true
			for _, file := range files {
				// println("readdir", path, file.Name())
				filenames[strings.ToLower(file.Name())] = struct{}{}
				var stat fuse.Stat_t
				if file.IsDir() {
					stat.Mode = fuse.S_IFDIR | 0777
				} else {
					stat.Mode = fuse.S_IFREG | 0777
					stat.Size = file.Size()
					stat.Mtim = fuse.NewTimespec(file.ModTime())
				}
				fill(file.Name(), &stat, 0)
				// println("fill", "overlay", file.Name())
			}
		} else if !os.IsNotExist(err) {
			println("failed to readdir", path, err)
		}
	}

	dirInfo, ok := fs.Directories[strings.ToLower(path)]

	if !ok {
		if !haveSomeFilesInOverlay {
			println("readdir: dir not found", path)
			return -fuse.ENOENT
		}
		return 0
	}

	for _, dir := range dirInfo.Directories {
		var stat fuse.Stat_t
		stat.Mode = fuse.S_IFDIR | 0777
		dirname := dir[strings.LastIndex(dir, "/")+1:]
		if _, ok := filenames[strings.ToLower(dirname)]; !ok {
			fill(dirname, &stat, 0)
			// println("fill", "dir", dirname)
		}
	}
	for _, file := range dirInfo.Files {
		file := fs.Files[strings.ToLower(file)]
		// println(file.Entry.Info.Path)
		var stat fuse.Stat_t
		GetFuseStatFromFileInfo(&file, &stat)
		filename := file.GetFilename()
		if _, ok := filenames[strings.ToLower(filename)]; !ok {
			fill(filename, &stat, 0)
			// println("fill", "file", filename)
		}
	}

	return 0
}

func (fs *MayakashiFS) Open(path string, flags int) (int, uint64) {
	defer recoverHandler()
	// println("open", path, flags)

	if strings.Contains(path, "/UnityCrashHandler64.exe") {
		return -fuse.ENOENT, 0
	}

	if overlayPath := fs.getOverlayPath(path); overlayPath != nil {
		nativeFlag := os.O_RDONLY
		mayWantsWrite := false
		if flags&fuse.O_WRONLY == fuse.O_WRONLY || flags&fuse.O_RDWR == fuse.O_RDWR {
			nativeFlag |= os.O_RDWR
			mayWantsWrite = true
		}
		if flags&fuse.O_APPEND == fuse.O_APPEND {
			nativeFlag |= os.O_APPEND
		}
		if mayWantsWrite {
			os.MkdirAll((*overlayPath)[:strings.LastIndex(*overlayPath, "/")], 0777)
		}
		fp, err := os.OpenFile(*overlayPath, nativeFlag, 0644)
		if err == nil {
			// println("open overlay", overlayPath, nativeFlag)
			fs.OverlayCount += 1
			oc := fs.OverlayCount
			fs.OverlayFileHandlers.Store(oc, &SharedFileHandler{
				File: fp,
			})
			return 0, oc
		}
		if !os.IsNotExist(err) {
			fmt.Println("failed to open overlay", path, err)
			return -fuse.EIO, 0
		}
	}

	if _, ok := fs.Files[strings.ToLower(path)]; ok {
		fs.Count += 1
		if flags != fuse.O_RDONLY {
			println("not O_RDONLY", path, flags)
			// TODO: We should copy it to overlay and open it.
			// return -fuse.EROFS, 0
		}
		// println("open", path)
		fs.LastDatRead = time.Now()
		return 0, uint64(fs.Count)
	}

	println("not found", path)
	return -fuse.ENOENT, 0
}

func (fs *MayakashiFS) Read(path string, buff []byte, offset int64, fh uint64) int {
	defer recoverHandler()
	readed := fs.readInternally(path, buff, offset, fh)
	if readed <= 0 {
		return readed
	}
	if readed < len(buff) {
		new_readed := fs.Read(path, buff[readed:], offset+int64(readed), fh)
		if new_readed < 0 {
			return new_readed
		}
		readed += new_readed
	}
	return readed
}

func (fs *MayakashiFS) readInternally(path string, buff []byte, offset int64, fh uint64) int {
	if fp, ok := fs.OverlayFileHandlers.Load(fh); ok {
		fp.Mutex.Lock()
		defer fp.Mutex.Unlock()
		fp.File.Seek(offset, 0)
		readed, err := fp.File.Read(buff)
		if err == io.EOF {
			return 0
		}
		if err != nil {
			fmt.Println("failed to ReadAt", err)
			return -fuse.EIO
		}
		// println("reading from overlay", path, offset, len(buff), readed)
		return readed
	}
	// println("read", path, offset, len(buff), fh)

	file, ok := fs.Files[strings.ToLower(path)]
	if !ok {
		println("read not found", path)
		return -fuse.ENOENT
	}

	if file.ZipEntry != nil {
		return fs.readInternalFromZipEntry(path, buff, offset, fh, &file)
	} else if file.MarEntry != nil {
		return fs.readInternalFromMarEntry(path, buff, offset, fh, &file)
	}

	fmt.Println("there is no known file entry", file)
	return -fuse.EIO
}

func (fs *MayakashiFS) readInternalFromZipEntry(path string, buff []byte, offset int64, fh uint64, file *FileInfo) int {
	entry := file.ZipEntry
	if offset >= entry.FileInfo().Size() {
		return 0
	}
	// If entry is not compressed, we can use OpenRaw() to read without decompressing, which reduces resource usage.
	if entry.Method == 0 {
		reader, err := entry.OpenRaw()
		if err != nil {
			fmt.Println("failed to open zip entry", err)
			return -fuse.EIO
		}
		r := reader.(io.ReadSeeker)
		_, err = r.Seek(offset, 0)
		if err != nil {
			fmt.Println("failed to seek zip entry", err)
			return -fuse.EIO
		}
		readed, err := r.Read(buff)
		if err == io.EOF {
			return 0
		}
		if err != nil {
			fmt.Println("failed to read zip (direct)", err)
			return -fuse.EIO
		}
		return readed
	}

	// check cache to avoid decompressing
	zipoffset, err := entry.DataOffset()
	if err != nil {
		fmt.Println("failed to get data offset", err)
		return -fuse.EIO
	}
	cache, ok := fs.ChunkCache.Get(fmt.Sprintf("%s#%d+%d", file.ArchiveFile, zipoffset, entry.CompressedSize64))
	if ok {
		decoded := cache.(*ChunkCache).Data
		readed := copy(buff, decoded[offset:])
		return readed
	}

	reader, err := entry.Open()
	if err != nil {
		fmt.Println("failed to open zip entry", err)
		return -fuse.EIO
	}
	defer reader.Close()

	dst := make([]byte, entry.UncompressedSize64)
	_, err = io.ReadFull(reader, dst)
	if err != nil {
		fmt.Println("failed to read zip data", err)
		return -fuse.EIO
	}

	fs.ChunkCache.Set(fmt.Sprintf("%s#%d+%d", file.ArchiveFile, zipoffset, entry.CompressedSize64), &ChunkCache{
		Data: dst,
	}, int64(len(dst)))

	readed := copy(buff, dst[offset:])

	return readed
}

func (fs *MayakashiFS) readInternalFromMarEntry(path string, buff []byte, offset int64, fh uint64, file *FileInfo) int {
	entry := file.MarEntry
	chunkStart := int64(0)
	datStart := int64(entry.BodyOffset)
	chunkNo := -1
	var targetChunk *pb.ChunkInfo
	for cn, chunk := range entry.Info.Chunks {
		if offset < (chunkStart + int64(chunk.OriginalLength)) {
			targetChunk = chunk
			chunkNo = cn
			// println("chunk number", cn, chunk.CompressedLength, chunk.OriginalLength, chunk.CompressedMethod, datStart)
			break
		}
		chunkStart += int64(chunk.OriginalLength)
		datStart += int64(chunk.CompressedLength)
	}

	if targetChunk == nil {
		// fmt.Println("chunk not found", path, offset, chunkStart)
		return 0
	}

	var marFileName string
	if entry.FileIndex == 0 {
		marFileName = file.ArchiveFile + ".dat"
	} else {
		marFileName = fmt.Sprintf("%s.%d.dat", file.ArchiveFile, entry.FileIndex)
	}

	pool := GetFilePoolFromPath(marFileName)

	if targetChunk.CompressedMethod == pb.CompressedMethod_ZSTANDARD {
		// println("zstd")
		cacheKey := fmt.Sprintf("%s#%d#%d", marFileName, datStart, chunkNo)
		cachedData, ok := fs.ChunkCache.Get(cacheKey)
		var decoded []byte
		if ok {
			// println("cache hit")
			decoded = cachedData.(*ChunkCache).Data
		} else {
			compressedBytes := make([]byte, targetChunk.CompressedLength)
			start := time.Now()
			fs.LastDatRead = start
			if _, err := pool.ReadAt(compressedBytes, datStart); err != nil {
				println("failed to ReadAt compressed data", err)
				return -fuse.EIO
			}
			used := time.Since(start)
			if used.Milliseconds() > 40 && fs.SlowReadLog != nil {
				fs.SlowReadLog.Write([]byte(path + "\n"))
			}

			decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
			if err != nil {
				println("failed to read", err)
				return -fuse.EIO
			}

			decoded, err = decoder.DecodeAll(compressedBytes, make([]byte, 0, int(targetChunk.OriginalLength)))
			if err != nil {
				println("failed to decode", err)
				return -fuse.EIO
			}

			fs.ChunkCache.Set(cacheKey, &ChunkCache{
				ChunkNo: chunkNo,
				Data:    decoded,
			}, int64(len(decoded)))
		}

		if offset < chunkStart {
			println("offset < chunkStart", path, offset, chunkStart)
			return -fuse.EIO
		}

		decoded = decoded[offset-chunkStart:]

		readed := copy(buff, decoded)

		// println("ok")

		return readed
	} else if targetChunk.CompressedMethod == pb.CompressedMethod_PASSTHROUGH {
		// println("passthrough", path)
		remainsLength := int(targetChunk.OriginalLength) - int(offset-chunkStart)
		if len(buff) > remainsLength {
			fmt.Println("!!!OVERLOAD!!!", len(buff), remainsLength)
			buff = buff[:remainsLength]
		}
		readed, err := pool.ReadAt(buff, datStart+(offset-chunkStart))
		if err != nil {
			fmt.Println("failed to read from passthrough", err)
			return -fuse.EIO
		}
		return readed
	} else {
		println("unknown compression method", targetChunk.CompressedMethod)
		return -fuse.EIO
	}
}

func (fs *MayakashiFS) Mkdir(path string, mode uint32) int {
	defer recoverHandler()
	println("mkdir", path, mode)
	overlayPath := fs.getOverlayPath(path)
	if overlayPath == nil {
		fmt.Println("mkdir requested but this path is not overlay")
		return -fuse.EROFS
	}
	err := os.MkdirAll(*overlayPath, 0777)
	if os.IsExist(err) {
		fmt.Println("mkdir requested but already exists", path)
		return -fuse.EEXIST
	}
	if err != nil {
		fmt.Println("failed to mkdir", err)
		return -fuse.EIO
	}
	return 0
}

func (fs *MayakashiFS) Create(path string, flags int, mode uint32) (int, uint64) {
	defer recoverHandler()
	overlayPath := fs.getOverlayPath(path)
	if overlayPath == nil {
		fmt.Println("tried to write read-only path", path)
		return -fuse.EROFS, 0
	}
	err := os.MkdirAll((*overlayPath)[:strings.LastIndex(*overlayPath, "/")], 0777)
	if err != nil {
		println("failed to mkdir for create", err)
		return -fuse.EIO, 0
	}
	println("create", path, flags, mode)
	file, err := os.Create(*overlayPath)
	if err != nil {
		println("failed to create", err)
		return -fuse.EIO, 0
	}
	fs.OverlayCount += 1
	oc := fs.OverlayCount
	fs.OverlayFileHandlers.Store(oc, &SharedFileHandler{
		File: file,
	})
	println("success", oc)
	return 0, oc
}

func (fs *MayakashiFS) Write(path string, buff []byte, offset int64, fh uint64) int {
	defer recoverHandler()
	// println("write", path, offset, len(buff), fh)
	file, ok := fs.OverlayFileHandlers.Load(fh)
	if !ok {
		fmt.Println("not writable", path)
		return -fuse.EROFS
	}
	file.Mutex.Lock()
	defer file.Mutex.Unlock()
	_, err := file.File.WriteAt(buff, offset)
	if err != nil {
		fmt.Println("failed to write", err)
		return -fuse.EIO
	}
	return len(buff)
}

func (fs *MayakashiFS) Release(path string, fh uint64) int {
	defer recoverHandler()
	// println("release", path, fh)
	if file, ok := fs.OverlayFileHandlers.Load(fh); ok {
		file.Mutex.Lock()
		defer file.Mutex.Unlock()
		file.File.Close()
		fs.OverlayFileHandlers.Delete(fh)
		if overlayPath, ok := fs.RemoveRequestedPaths.Load(strings.ToLower(path)); ok {
			err := os.Remove(overlayPath)
			if err == nil {
				fmt.Println("successfly remove scheduled files: ", path)
				fs.RemoveRequestedPaths.Delete(strings.ToLower(path))
			} else {
				fmt.Println("try to remove scheduled files: failed to remove", path, err)
			}
		}
		if overlayPath, ok := fs.RenameRequestedPaths.Load(strings.ToLower(path)); ok {
			err := os.Rename(overlayPath.OldPath, overlayPath.NewPath)
			if err == nil {
				fmt.Println("successfly rename scheduled files: ", path)
				fs.RenameRequestedPaths.Delete(strings.ToLower(path))
			} else {
				fmt.Println("try to rename scheduled files: failed to rename", path, err)
			}
		}
	}
	return 0
}

func (fs *MayakashiFS) Access(path string, mask uint32) int {
	defer recoverHandler()
	// println("access", path, mask)
	return 0
}

func (fs *MayakashiFS) Unlink(path string) int {
	defer recoverHandler()
	if overlayPath := fs.getOverlayPath(path); overlayPath != nil {
		err := os.Remove(*overlayPath)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			fmt.Println("failed to remove, scheduled", err)
			fs.RemoveRequestedPaths.Store(strings.ToLower(path), *overlayPath)
		}
		return 0
	}

	fmt.Println("tried to remove but read-only", path)
	return -fuse.EROFS
}

func (fs *MayakashiFS) Rename(oldpath_in_fuse string, newpath_in_fuse string) int {
	defer recoverHandler()
	oldPath := fs.getOverlayPath(oldpath_in_fuse)
	if oldPath == nil {
		fmt.Println("tried to rename but oldpath is read-only", oldpath_in_fuse, newpath_in_fuse)
		return -fuse.EROFS
	}
	newPath := fs.getOverlayPath(newpath_in_fuse)
	if newPath == nil {
		fmt.Println("tried to rename but newpath is read-only", oldpath_in_fuse, newpath_in_fuse)
		return -fuse.EROFS
	}
	err := os.Rename(*oldPath, *newPath)
	if err != nil {
		if os.IsPermission(err) {
			fmt.Println("tried to rename but read-only", oldpath_in_fuse, newpath_in_fuse)
			return -fuse.EPERM
		}
		fmt.Println("failed to rename, queued", err)
		fs.RenameRequestedPaths.Store(strings.ToLower(oldpath_in_fuse), RenameRequest{
			OldPath: *oldPath,
			NewPath: *newPath,
		})
		return 0
	}

	return 0
}

func (fs *MayakashiFS) Truncate(path string, size int64, fh uint64) int {
	if fp, ok := fs.OverlayFileHandlers.Load(fh); ok {
		fp.Mutex.Lock()
		defer fp.Mutex.Unlock()
		err := fp.File.Truncate(size)
		if err != nil {
			fmt.Println("failed to truncate", err)
			return -fuse.EIO
		}

		return 0
	}
	return -fuse.EROFS
}

func main() {
	fmt.Println(runtime.GOARCH)

	fs := NewMayakashiFS()
	fs.OverlayDir = "overlay"
	fuseOpts := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			fuseOpts = os.Args[i+1:]
			break
		}
		if i == 0 {
			continue
		}
		if err := fs.ParseFile(arg); err != nil {
			panic(err)
		}
	}
	if runtime.GOOS == "windows" {
		fuseOpts = append([]string{"-o", "uid=-1", "-o", "gid=-1"}, fuseOpts...)
	}
	// pp.Print(fs.Directories)
	// return

	go func() {
		type RuleAndFile struct {
			Rule     string
			FileName string
		}
		preloadFilesPerMarFile := map[string][]RuleAndFile{}
		for _, rule := range fs.PreloadGlobs {
			for filename, file := range fs.Files {
				matched, err := doublestar.Match(strings.ToLower(rule), filename)
				if err != nil {
					panic(err)
				}
				if !matched {
					continue
				}
				var marFileName string
				entry := file.MarEntry
				if entry == nil {
					continue
				}
				if entry.FileIndex == 0 {
					marFileName = file.ArchiveFile + ".dat"
				} else {
					marFileName = fmt.Sprintf("%s.%d.dat", file.ArchiveFile, entry.FileIndex)
				}
				if _, ok := preloadFilesPerMarFile[marFileName]; !ok {
					preloadFilesPerMarFile[marFileName] = []RuleAndFile{}
				}
				preloadFilesPerMarFile[marFileName] = append(preloadFilesPerMarFile[marFileName], RuleAndFile{
					Rule:     rule,
					FileName: filename,
				})
			}
		}

		for marFileName, files := range preloadFilesPerMarFile {
			go func(marFileName string, files []RuleAndFile) {
				for _, f := range files {
					rule := f.Rule
					filename := f.FileName
					fmt.Println("matched", rule, marFileName, filename)
					file := fs.Files[strings.ToLower(filename)]
					pool := GetFilePoolFromPath(marFileName)
					ptr := file.MarEntry.BodyOffset
					for _, chunk := range file.MarEntry.Info.Chunks {
						first_wait := true
						for fs.LastDatRead.Add(3 * time.Second).After(time.Now()) {
							fmt.Println("waiting for dat read", filename, fs.LastDatRead)
							first_wait = false
							time.Sleep(1 * time.Second)
						}
						if !first_wait {
							fmt.Println("continue...")
						}
						pool.ReadAt(make([]byte, chunk.CompressedLength), int64(ptr))
						ptr += uint64(chunk.CompressedLength)
					}
				}
				println("preload finish", marFileName)
			}(marFileName, files)
		}
	}()

	host := fuse.NewFileSystemHost(fs)
	host.SetCapCaseInsensitive(true)
	if fs.PProfAddr != "" {
		go func() {
			http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("Hello."))
			})
			log.Fatal(http.ListenAndServe(fs.PProfAddr, nil))
		}()
	}
	host.Mount("", fuseOpts)
}
