extern crate prost_build;

fn main() {
    println!("cargo:rerun-if-changed=proto/mayakashi.proto");
    prost_build::compile_protos(&["proto/mayakashi.proto"], &["proto/"]).unwrap();
}