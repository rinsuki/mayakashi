package main

import (
	"fmt"
	"os"
	"sync"
)

const FILE_POOL_LIMIT = 8

type FilePool struct {
	filePools          []*os.File
	currentlyUsedFiles int
	lock               sync.Mutex
	filePath           string
}

var filePools map[string]*FilePool = map[string]*FilePool{}
var filePoolRWLock sync.RWMutex

func GetFilePoolFromPath(path string) *FilePool {
	filePoolRWLock.RLock()
	fp, ok := filePools[path]
	filePoolRWLock.RUnlock()
	if ok {
		return fp
	}
	filePoolRWLock.Lock()
	fp, ok = filePools[path]
	if !ok {
		fp = NewFilePool(path)
		filePools[path] = fp
	}
	filePoolRWLock.Unlock()
	return fp
}

func NewFilePool(path string) *FilePool {
	pools := []*os.File{}
	for i := 0; i < (FILE_POOL_LIMIT / 2); i++ {
		f, err := os.Open(path)
		if err != nil {
			panic(err)
		}
		pools = append(pools, f)
	}

	return &FilePool{
		lock:      sync.Mutex{},
		filePath:  path,
		filePools: pools,
	}
}

func (fp *FilePool) GetOne() (*os.File, error) {
	fp.lock.Lock()
	defer fp.lock.Unlock()

	var f *os.File
	if len(fp.filePools) < 1 {
		fmt.Println("creating new os.File for ", fp.filePath, "count", fp.currentlyUsedFiles)
		var err error
		f, err = os.Open(fp.filePath)
		if err != nil {
			fmt.Println("error opening file for pool, path:", fp.filePath)
			return nil, err
		}
	} else {
		// fmt.Println("reusing os.File for ", fp.filePath)
		f = fp.filePools[0]
		fp.filePools = fp.filePools[1:]
	}

	fp.currentlyUsedFiles++
	return f, nil
}

func (fp *FilePool) ReturnOne(f *os.File) {
	fp.lock.Lock()
	defer fp.lock.Unlock()

	fp.currentlyUsedFiles--
	fp.filePools = append(fp.filePools, f)
}

func (fp *FilePool) ReadAt(b []byte, off int64) (n int, err error) {
	f, err := fp.GetOne()
	if err != nil {
		return 0, err
	}
	defer fp.ReturnOne(f)

	return f.ReadAt(b, off)
}
