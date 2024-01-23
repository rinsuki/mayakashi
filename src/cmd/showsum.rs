use std::path::PathBuf;

use clap::Parser;


#[derive(Parser)]
#[command(name = "MAR Maker")]
pub struct Args {
    #[arg(short, long)]
    input: PathBuf,
}

pub fn main(args: Args) {
    let mut file = std::fs::File::open(args.input).unwrap();
    let file = crate::format::index_file::parse_index_file(&mut file);
    for entry in file.entries {
        let info = entry.info.unwrap();
        let sha256 = info.original_sha256;
        // convert sha256 to hex
        let mut hex = String::new();
        for byte in sha256 {
            hex.push_str(&format!("{:02x}", byte));
        }
        println!("{}\t{}", hex, info.path);
    }
}