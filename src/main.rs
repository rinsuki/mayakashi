use clap::{Parser, Subcommand};

mod proto;
mod cmd;
mod format;

#[derive(Parser)]
struct Cli {
    #[clap(subcommand)]
    subcommand: SubCommands,
}

#[derive(Subcommand)]
enum SubCommands {
    Create(cmd::create::Args),
    ShowSum(cmd::showsum::Args),
    Split(cmd::split::Args),
}

fn main() {
    let cli = Cli::parse();
    match cli.subcommand {
        SubCommands::Create(args) => cmd::create::main(args),
        SubCommands::ShowSum(args) => cmd::showsum::main(args),
        SubCommands::Split(args) => cmd::split::main(args),
    }
}
