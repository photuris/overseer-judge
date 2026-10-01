//! Entry point for overseer-judge: installs the SIGINT handler and runs
//! the CLI.

use std::io;

use overseer_judge::cli;

/// Installs the SIGINT handler, then exits with [`cli::run`]'s code, or
/// 130 once SIGINT has been seen.
fn main() {
    // Without a handler, SIGINT still ends the process, only without
    // the record and with the signal's own status; nothing to recover.
    let _ = ctrlc::set_handler(|| cli::interrupt_exit(&mut io::stderr()));

    let code = cli::run(
        std::env::args_os().skip(1).collect(),
        &mut io::stdin(),
        &mut io::stdout(),
        &mut io::stderr(),
    );

    cli::exit(code, &mut io::stderr());
}
