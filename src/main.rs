//! Entry point for overseer-judge: installs the SIGINT handler and runs
//! the CLI.

use std::{io, process};

/// The error record written when SIGINT arrives.
const INTERRUPTED: &str =
    "{\"error\":{\"type\":\"interrupted\",\"message\":\"interrupted\"}}\n";

/// Installs the SIGINT handler, then exits with [`overseer_judge::cli::run`]'s code.
fn main() {
    // Without a handler, SIGINT still ends the process, only without
    // the record and with the signal's own status; nothing to recover.
    let _ = ctrlc::set_handler(|| {
        // The process is exiting either way; a failed write changes
        // nothing.
        let _ =
            io::Write::write_all(&mut io::stderr(), INTERRUPTED.as_bytes());
        process::exit(130);
    });

    process::exit(overseer_judge::cli::run(
        std::env::args_os().skip(1).collect(),
        &mut io::stdin(),
        &mut io::stdout(),
        &mut io::stderr(),
    ));
}
