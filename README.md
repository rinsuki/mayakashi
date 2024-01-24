# Mayakashi

Make read-only compressed archive which is capable of random access (per chunk), called "MAR" (**M**ayakashi **AR**chive).

also supports mount archive with read-write overlay directory option.

now, time to compress games data file, remove tons of low-entropy data (e.g. storing texture as RGBA8888 without even LZ4) from your SSD.

## Requirements

* 9999GHz Intel i9999 or Apple M99999 Ultra
  * since Zstandard decompression is pretty fast, you might be not needed to have that much powerful CPU
* tons of RAM (depends to your games size)

## TODO

- [ ] Stop to hide `UnityCrashHandler64.exe` in marmounter
- [ ] Handle Overwrite to archived files (currently it returns EROFS, but it should be copy to overlay and open it)

## Usage

there is two part:
* Rust part
  * builds .mar.* archive.
  * you can run with `cargo run --release --`
* Go part
  * mounts .mar.* archive, powered by https://github.com/winfsp/cgofuse
  * you can run with `go run ./marmounter`

### marmounter options

* `onlyglob=<glob>:...`
  * Only mount files which matches this glob pattern (e.g. `onlyglob=*.png:some.mar`)
  * You can specify multiple glob patterns by separating with `:`
  * NOTE: addprefix and stripprefix will not applied to this glob pattern
  * NOTE: case insensitive
* `stripprefix=<prefix>:...`
  * Strip prefix from path if it starts with this prefix
  * If file will not start with this prefix, it will be not touched (still remains)
    * If you want to remove those files, you should use `onlyglob` option
  * NOTE: addprefix will not applied to this
  * NOTE: case insensitive
* `addprefix=<prefix>:...`
  * Add prefix to all files in archive
  * e.g. `addprefix=foo/bar:some.mar` will add `foo/bar` prefix to all files in `some.mar`
* `roprefix=<prefix>`
  * If path starts with this prefix, we wouldn't check overlay directory
* `overlaydir=<dir>` 
  * Overlay directory path (default: `./overlay`)
* `commandsfile=<file>`
  * Read options from this file (one option per line)
* `preload=<glob>`
   * Preload chunks which matches this glob pattern (e.g. `preload=*.png`)
   * This is useful if you are using remote filesystem with caching mechanism to local storage, like Rclone
   * NOTE: Actual decompress will not proceed by preload
* `pprof=<addr>`
  * Enable pprof on this address (e.g. `pprof=:6060`)
* `/path/to/file.zip`
  * Mount zip file
  * NOTE: Reading big file from zip file will be slow, you should consider to use .mar file if zip contains large file
  * (It would be still useful for small files, like small mods .zip file)
* `/path/to/file.mar`
  * Mount MAR file
  * You should have `file.mar.idx` and `file.mar.dat` in your directory

### Q. Why you are using Go if you also write Rust

because FUSE on Rust program which supports multi-platform would be nightmare:

| crate | Linux | macOS | Windows | note |
| --- | --- | --- | --- | --- |
| [fuse](https://github.com/zargony/fuse-rs) | :heavy_check_mark: | :heavy_check_mark: | :x: | last commit is 2020 |
| [async-fuse](https://github.com/udoprog/async-fuse) | :heavy_check_mark: | :x: | :x: | supported platforms are from GHA configuration |
| [fuse-backend-rs](https://github.com/cloud-hypervisor/fuse-backend-rs) | :heavy_check_mark: | :heavy_check_mark: | :x: | supported platforms are from GHA configuration |
| [fuser](https://github.com/cberner/fuser) | :heavy_check_mark: | :heavy_check_mark: (they says untested) | :x: | |
| [winfsp](https://github.com/SnowflakePowered/winfsp-rs) | :x: | :x: | :heavy_check_mark: | |

since I'm writing code on macOS and running most games on Windows (Wine on Apple Silicon Mac is yet another nightmare), I need to support both of them.

please, someone, make a multi-platform FUSE client crate for me 🥺
