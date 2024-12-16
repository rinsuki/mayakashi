use std::{collections::{HashMap, HashSet}, io::{Read, Seek, SeekFrom, Write}, path::PathBuf};

use clap::Parser;

use crate::{format::index_file::write_index_file, proto::{self, FileEntry}};

#[derive(Parser)]
#[command(name = "MAR Splitter")]
pub struct Args {
    #[arg(short, long)]
    input: PathBuf,

    #[arg(short, long)]
    count: usize,
}

struct ChunkedFile {
    entries: Vec<FileEntry>,
    well_known_hashes: HashSet<Vec<u8>>,
    size: u64,
}

impl ChunkedFile {
    fn new() -> Self {
        Self {
            entries: Vec::new(),
            size: 0,
            well_known_hashes: HashSet::new(),
        }
    }

    fn add(&mut self, entry: FileEntry) {
        if !self.well_known_hashes.contains(&entry.info.clone().unwrap().original_sha256) {
            self.well_known_hashes.insert(entry.info.clone().unwrap().original_sha256);
            for c in entry.info.clone().unwrap().chunks {
                self.size += c.compressed_length as u64;
            }
        }
        self.entries.push(entry);
    }
}

// "/a/b/c", "d" => "/a/b/cd"
fn append_to_path(p: &PathBuf, s: &str) -> PathBuf {
    let mut p = p.clone().into_os_string();
    p.push(s);
    p.into()
}

pub fn main(args: Args) {
    let entries = {
        let mut f = std::fs::File::open(append_to_path(&args.input, ".idx")).unwrap();
        let file = crate::format::index_file::parse_index_file(&mut f);
        let mut entries = file.entries;
        // sort by all chunks size
        entries.sort_by_cached_key(|e| e.info.clone().unwrap().chunks.into_iter().map(|c| match c.compressed_length {
            0 => c.original_length,
            _ => c.compressed_length,
        } as u64).sum::<u64>());
        entries.reverse();
        entries
    };


    let mut already_well_known_hahes = HashMap::<Vec<u8>, usize>::new();

    let mut output_files = Vec::<ChunkedFile>::with_capacity(args.count);
    for _ in 0..args.count {
        output_files.push(ChunkedFile::new());
    }

    for entry in entries {
        let hash = entry.info.clone().unwrap().original_sha256;
        match already_well_known_hahes.get(&hash) {
            Some(index) => {
                output_files[*index].add(entry);
                continue;
            }
            None => {}
        }
        let mut min_size = u64::MAX;
        let mut min_index = 0;
        for (i, file) in output_files.iter().enumerate() {
            if file.size < min_size {
                min_size = file.size;
                min_index = i;
            }
        }
        output_files[min_index].add(entry);
        already_well_known_hahes.insert(hash, min_index);
    }
    
    let mut all_size = 0;

    for (i, file) in output_files.iter_mut().rev().enumerate() {
        println!("Writing file {}, {}MB", i, file.size / 1024 / 1024);
        all_size += file.size;
        println!("Total size: {} MB", all_size / 1024 / 1024);

        let mut idxfile = std::fs::File::create(append_to_path(&args.input, format!(".split.{}.mar.idx", i).as_str())).unwrap();
        let mut datfile = std::fs::File::create(append_to_path(&args.input, format!(".split.{}.mar.dat", i).as_str())).unwrap();

        file.entries.reverse();

        let mut offset = 0;
        // let mut files = Vec::new();
        struct Entry {
            offset: u64,
            size: u64,
        }
        let mut well_known_hashes = HashMap::new();

        let mut out_entries = Vec::<FileEntry>::new();

        for in_entry in &file.entries {
            let out_entry = match well_known_hashes.get(&in_entry.info.clone().unwrap().original_sha256) {
                Some(offset) => {
                    FileEntry {
                        info: Some(in_entry.info.clone().unwrap()),
                        file_index: 0,
                        body_offset: *offset as u64,
                        body_size: in_entry.body_size,
                    }
                }
                None => {
                    let info = in_entry.info.as_ref().unwrap();
                    let current_offset = datfile.seek(SeekFrom::Current(0)).unwrap();
                    let mut written: u64 = 0;
        
                    let mut srcdat = std::fs::File::open(append_to_path(&args.input, ".dat")).unwrap();
                    srcdat.seek(SeekFrom::Start(in_entry.body_offset)).unwrap();
        
                    for chunk in &info.chunks {
                        let mut buf = vec![0; chunk.compressed_length as usize];
                        srcdat.read_exact(&mut buf).unwrap();
                        datfile.write_all(&buf).unwrap();
                        written += buf.len() as u64;
                    }
        
                    let entry = FileEntry {
                        info: Some(info.clone()),
                        file_index: 0,
                        body_offset: current_offset,
                        body_size: written,
                    };

                    assert_eq!(in_entry.body_size, written);

                    well_known_hashes.insert(info.original_sha256.clone(), current_offset);
                    entry
                }
            };

            out_entries.push(out_entry);
        }

        write_index_file(proto::FileIndexFile { entries: out_entries }, &mut idxfile);
    }
}