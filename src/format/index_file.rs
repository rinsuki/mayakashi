use std::io::Read;

use prost::Message;

use crate::proto;

const INDEX_MAGIC: &[u8; 4] = b"MARI";

pub fn parse_index_file(input: &mut impl Read) -> proto::FileIndexFile {
    // first 4 bytes: INDEX_MAGIC
    // next 4 bytes: compressed length (big-endian)
    // next 4 bytes: raw length (big-endian)
    // (data)

    let mut magic = [0; 4];
    input.read_exact(&mut magic).unwrap();
    assert_eq!(&magic, INDEX_MAGIC);

    let mut compressed_len = [0; 4];
    input.read_exact(&mut compressed_len).unwrap();
    let compressed_len = u32::from_be_bytes(compressed_len);

    let mut raw_len = [0; 4];
    input.read_exact(&mut raw_len).unwrap();
    let raw_len = u32::from_be_bytes(raw_len);

    let mut compressed = Vec::with_capacity(compressed_len as usize);
    let mut l = input.take(compressed_len as u64);
    l.read_to_end(&mut compressed).unwrap();

    let raw = zstd::decode_all(&compressed[..]).unwrap();
    assert_eq!(raw.len(), raw_len as usize);

    return proto::FileIndexFile::decode(&raw[..]).unwrap();
}