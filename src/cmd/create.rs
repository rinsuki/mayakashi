use std::{collections::{BTreeMap, HashMap, HashSet, VecDeque}, ffi::OsString, io::{Seek, Write}, path::PathBuf, sync::{Arc, Mutex}, thread};

use prost::Message;
use clap::Parser;

use crate::proto::{self, CompressedMethod};

#[derive(Parser)]
#[command(name = "MAR Maker")]
pub struct Args {
    #[arg(short, long)]
    input: PathBuf,
    
    #[arg(short, long)]
    output: PathBuf,

    #[arg(short, long)]
    jobs: usize,

    #[arg(long)]
    dedup: bool,
}

#[derive(Debug)]
struct FileInfo {
    path: PathBuf,
    size: u64,
}


fn walk_dir(dir: &PathBuf) -> (Vec<FileInfo>, Vec<PathBuf>) {
    let mut files = Vec::new();
    let mut directories = Vec::new();
    for entry in dir.read_dir().unwrap() {
        let entry = entry.unwrap();
        let path = entry.path();
        if path.is_dir() {
            let (mut f, mut d) = walk_dir(&path);
            directories.push(path);
            directories.append(&mut d);
            files.append(&mut f);
        } else {
            files.push(FileInfo { path: entry.path(), size: entry.metadata().unwrap().len() });
        }
    }
    return (files, directories);
}

const CHUNK_SIZE: usize = 4 * 1024 * 1024;

struct Chunk {
    start: usize,
    original_size: usize,
    compressed: Vec<u8>,
    compressed_method: CompressedMethod,
    // using_dictionary: bool,
}

fn compress_file(input_data: &[u8]) -> Vec<Chunk> {
    // input_data を Zstandard で圧縮したもの
    let compressed_with_zstd = {
        let mut buf = Vec::<u8>::with_capacity(input_data.len() * 2);
        let mut encoder = zstd::Encoder::new(&mut buf, 22).unwrap();
        encoder.write_all(&input_data).unwrap();
        encoder.finish().unwrap();
        buf
    };

    // 入力サイズが 8 chunk 以下、あるいは Zstandard の圧縮結果が 2 chunk 以下の時はチャンク毎圧縮をしない (十分に小さいためシーク時の遅さを気にする必要がない…ことにする)
    if input_data.len() <= (CHUNK_SIZE * 8) || compressed_with_zstd.len() <= (CHUNK_SIZE * 2) {
        // 圧縮成功したら圧縮したものを返す、そうでなかったらパススルー
        if input_data.len() > compressed_with_zstd.len() {
            return vec![Chunk {
                start: 0,
                original_size: input_data.len(),
                compressed: compressed_with_zstd,
                compressed_method: CompressedMethod::Zstandard,
                // using_dictionary: false,
            }];
        } else {
            return vec![Chunk {
                start: 0,
                original_size: input_data.len(),
                compressed: input_data.to_vec(),
                compressed_method: CompressedMethod::Passthrough,
                // using_dictionary: false,
            }];
        }
    }

    // 入力データを CHUNK_SIZE ずつに分割して圧縮する
    let mut chunks = Vec::<Chunk>::new();
    for i in (0..input_data.len()).step_by(CHUNK_SIZE) {
        // 範囲を取得
        let end = (i + CHUNK_SIZE).min(input_data.len());
        // 最後のチャンクがあった場合、それを拡張できないか考える
        let last_chunk = chunks.last_mut();
        if let Some(last_chunk) = last_chunk {
            // 最終チャンクが Passthrough でなく、かつ圧縮後チャンクサイズが 3/4 以下の場合は拡張する
            if last_chunk.compressed_method != CompressedMethod::Passthrough && last_chunk.compressed.len() < ((CHUNK_SIZE / 4) * 3) {
                let extended_compressed = {
                    let mut buf = Vec::<u8>::with_capacity(CHUNK_SIZE * 2);
                    let mut encoder = zstd::Encoder::new(&mut buf, 22).unwrap();
                    encoder.write_all(&input_data[last_chunk.start..end]).unwrap();
                    encoder.finish().unwrap();
                    buf
                };
                if extended_compressed.len() < CHUNK_SIZE {
                    last_chunk.compressed_method = CompressedMethod::Zstandard;
                    last_chunk.original_size = end - last_chunk.start;
                    last_chunk.compressed = extended_compressed;
                    // last_chunk.using_dictionary = false;
                    continue;
                }
            }
        }
        // 最終チャンクを拡張できなかった (あるいは最終チャンクがない) 場合、新しいチャンクを作る
        let src = &input_data[i..end];
        let compressed = {
            let mut buf = Vec::<u8>::with_capacity(CHUNK_SIZE * 2);
            let mut encoder = zstd::Encoder::new(&mut buf, 22).unwrap();
            encoder.write_all(src).unwrap();
            encoder.finish().unwrap();
            buf
        };

        let is_compressed = compressed.len() < (src.len() / 4 * 3);

        chunks.push(if is_compressed {
            // 圧縮できた
            Chunk {
                start: i,
                original_size: src.len(),
                compressed,
                compressed_method: CompressedMethod::Zstandard,
                // using_dictionary: false,
            }
        } else {
            // 圧縮できなかった
            Chunk {
                start: i,
                original_size: src.len(),
                compressed: src.to_vec(),
                compressed_method: CompressedMethod::Passthrough,
                // using_dictionary: false,
            }
        });
    };

    return chunks;
}


pub fn main(args: Args) {
    let (mut files, directories) = walk_dir(&args.input);
    files.sort_by_key(|f| std::cmp::Reverse(f.size));
    // println!("Files: {:#?}", files);

    let files_count: usize = files.len();

    let workload = Arc::new(Mutex::new(VecDeque::from(files)));
    let outfilestr = args.output.into_os_string();
    let outdatfile = Arc::new(Mutex::new(std::fs::File::create({
        let mut outfile = OsString::from(&outfilestr);
        outfile.push(".mar.dat");
        println!("Output: {}", outfile.to_str().unwrap());
        outfile
    }).unwrap()));
    let mut outidxfile = std::fs::File::create({
        let mut outfile = OsString::from(&outfilestr);
        outfile.push(".mar.idx");
        outfile
    }).unwrap();

    // make ${input.jobs} threads

    let enc_start = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_millis();

    let mut threads = Vec::new();

    let hash_to_offsets = Arc::new(Mutex::new(HashMap::<Vec<u8>, proto::FileEntry>::new()));

    struct PartialFileInfo {
        path: String,
        modified_time: Option<prost_types::Timestamp>,
        original_crc32: u32,
        original_sha256: Vec<u8>,
    }

    let mut already_well_known_hashes = Arc::new(Mutex::new(HashSet::<Vec<u8>>::new()));
    let mut deduped_file_entries = Arc::new(Mutex::new(Vec::<PartialFileInfo>::new()));

    for thread_no in 0..args.jobs {
        let workload = workload.clone();
        let input = args.input.to_str().unwrap().to_string();
        let outdatfile = outdatfile.clone();
        let hash_to_offsets = hash_to_offsets.clone();
        let already_well_known_hashes = already_well_known_hashes.clone();
        let deduped_file_entries = deduped_file_entries.clone();

        threads.push(thread::spawn(move || {
            let mut entries = Vec::new();
            loop {
                let workload = workload.lock().unwrap().pop_front();
                if let Some(file) = workload {
                    if file.path.file_name().unwrap() == ".DS_Store" {
                        continue;
                    }

                    let mut fp: std::fs::File = std::fs::File::open(&file.path).unwrap();
                    let metadata = fp.metadata().unwrap();
                    let input_data = {
                        let mut data = Vec::<u8>::with_capacity(metadata.len() as usize);
                        std::io::copy(&mut fp, &mut data).unwrap();
                
                        data
                    };

                    let relative_path = file.path.to_str().unwrap();
                    assert!(relative_path.starts_with(&input));
                    let relative_path = relative_path[input.len()..].to_string();

                    let modified_time = fp.metadata().unwrap().modified().unwrap();

                    let original_crc32 = crc32fast::hash(&input_data);
                    let original_sha256 = sha2::Sha256::digest(&input_data).to_vec();

                    // もしもう圧縮済みの同 SHA-256 ファイルがあればそちらを使う
                    if args.dedup {
                        let mut already_well_known_hashes = already_well_known_hashes.lock().unwrap();
                        if already_well_known_hashes.contains(&original_sha256) {
                            println!("dedup {}", relative_path);
                            let mut deduped_file_entries = deduped_file_entries.lock().unwrap();
                            deduped_file_entries.push(PartialFileInfo {
                                path: relative_path.clone(),
                                modified_time: Some(prost_types::Timestamp::from(modified_time)),
                                original_crc32,
                                original_sha256,
                            });
                            continue;
                        }
                        already_well_known_hashes.insert(original_sha256.clone());
                    }

                    let chunks = compress_file(&input_data);

                    let mut chunk_infos = Vec::<proto::ChunkInfo>::with_capacity(chunks.len());
                    let mut compressed = Vec::new();
                    for mut chunk in chunks {
                        chunk_infos.push(proto::ChunkInfo {
                            compressed_length: chunk.compressed.len() as u32,
                            compressed_method: chunk.compressed_method as i32,
                            original_length: chunk.original_size as u32,
                        });
                        compressed.append(&mut chunk.compressed);
                    }
                    println!("{}: {} ({} chunks, {} -> {} bytes)", thread_no, relative_path, chunk_infos.len(), input_data.len(), compressed.len());

                    use sha2::Digest;

                    let entry = {
                        let mut hash_to_offsets = hash_to_offsets.lock().unwrap();

                        let file_info = proto::FileInfo {
                            path: relative_path,
                            chunks: chunk_infos,
    
                            chunks_crc32: crc32fast::hash(&compressed),
                            chunks_sha256: sha2::Sha256::digest(&compressed).to_vec(),
    
                            original_crc32,
                            original_sha256,
    
                            modified_time: Some(prost_types::Timestamp::from(modified_time)),
                            // dictionary_size: 0,
                            priority: 0,
                        };

                        let offset = {
                            let mut outdatfile = outdatfile.lock().unwrap();
                            let offset = outdatfile.seek(std::io::SeekFrom::End(0)).unwrap();
                            outdatfile.write_all(&compressed).unwrap();

                            offset
                        };

                        let entry = proto::FileEntry {
                            info: Some(file_info),
                            file_index: 0,
                            body_offset: offset,
                            body_size: compressed.len() as u64,
                        };

                        if args.dedup {
                            hash_to_offsets.insert(entry.info.as_ref().unwrap().original_sha256.clone(), entry.clone());
                        }

                        entry
                    };

                    entries.push(entry);
                } else {
                    break entries;
                }
            }
        }));
    }

    let mut entries = Vec::<proto::FileEntry>::with_capacity(files_count);

    let mut ees = Vec::with_capacity(files_count);
    for thread in threads {
        for e in thread.join().unwrap() {
            // entries.push(FileEntry {
            //     path: e.path.to_str().unwrap().to_string(),
            //     compressed_method: e.compressed_method,
            //     dictionary_id: None,
            //     compressed_size: e.compressed_size,
            //     original_size: e.compressed_size,
            //     compressed_crc32: e.compressed_crc32,
            //     original_crc32: e.original_crc32,
            // });
            ees.push(e);
        }
    }

    let hash_to_offsets = hash_to_offsets.lock().unwrap();
    for e in deduped_file_entries.lock().unwrap().drain(0..) {
        let dedup_target = hash_to_offsets.get(&e.original_sha256).unwrap().clone();
        assert!(dedup_target.info.as_ref().unwrap().original_sha256 == e.original_sha256);
        assert!(dedup_target.info.as_ref().unwrap().original_crc32 == e.original_crc32);
        ees.push(proto::FileEntry {
            info: Some(proto::FileInfo {
                path: e.path,
                modified_time: e.modified_time,
                ..dedup_target.info.as_ref().unwrap().clone()
            }),
            ..dedup_target
        });
    }

    let enc_end = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_millis();

    let dec_start = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_millis();
    ees.sort_by(|a, b| a.info.as_ref().unwrap().path.cmp(&b.info.as_ref().unwrap().path));
    let index_file = proto::FileIndexFile {
        entries: ees,
    };
    let index_file_bytes = index_file.encode_to_vec();
    let index_file_len = index_file_bytes.len();
    let index_file_bytes = zstd::encode_all(&index_file_bytes[..], 22).unwrap();

    outidxfile.write_all(b"MARI").unwrap();
    outidxfile.write_all(&(index_file_bytes.len() as u32).to_be_bytes()).unwrap();
    outidxfile.write_all(&(index_file_len as u32).to_be_bytes()).unwrap();
    outidxfile.write_all(&index_file_bytes).unwrap();

    let dec_end = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_millis();
    println!("{},{}", enc_end - enc_start, dec_end - dec_start);
}
