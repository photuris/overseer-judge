//! Command-line interface: clap types, verb dispatch, exit codes, and the stderr error record.
//!
//! [`run`] is the whole program: `main` only installs the SIGINT
//! handler and exits with its return value. stdout carries data only;
//! every failure ends stderr with one JSON error record.

use std::{
    collections::BTreeMap,
    env,
    ffi::OsString,
    fs,
    io::{self, Read, Write},
    iter, process,
    sync::{
        Mutex, PoisonError,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use clap::{
    ArgAction, Args, Parser, Subcommand, ValueEnum,
    builder::{PossibleValue, PossibleValuesParser, TypedValueParser},
    error::ErrorKind,
};
use serde::Serialize;
use serde_json::value::RawValue;
use tracing::{Level, debug, warn};

use crate::{
    config::{self, Config},
    jev, review,
    session::{self, AgentKind},
    tasklint,
};

// ── Exit codes ──────────────────────────────────────────────────────────────

/// Success.
const EXIT_OK: i32 = 0;
/// An unexpected failure, such as stdout closing mid-write.
const EXIT_UNKNOWN: i32 = 1;
/// Bad flags, a missing argument, or bad input.
const EXIT_USAGE: i32 = 2;
/// Authentication failed or no API key.
const EXIT_AUTH: i32 = 3;
/// The request was rejected (422 or another 4xx).
const EXIT_REQUEST: i32 = 5;
/// Rate limited after the client's retries.
const EXIT_RATE_LIMIT: i32 = 6;
/// A 5xx after retries, or an invalid response.
const EXIT_UPSTREAM: i32 = 7;
/// A network failure or timeout.
const EXIT_NETWORK: i32 = 8;
/// SIGINT arrived.
pub const EXIT_INTERRUPTED: i32 = 130;

// ── Interrupt ───────────────────────────────────────────────────────────────

/// Set by the SIGINT handler before anything else, so a failure the
/// signal caused (such as EINTR) is reported as the interrupt.
pub static INTERRUPTED: AtomicBool = AtomicBool::new(false);

/// Claimed by whichever thread writes the `interrupted` record, so it
/// is written once.
pub static REPORTED: AtomicBool = AtomicBool::new(false);

/// Held across every whole stdout record and every stderr error
/// record, and by [`interrupt_exit`] until the process is gone, so an
/// exit can only fall between complete records. Never held across a
/// read or a request.
pub static OUTPUT: Mutex<()> = Mutex::new(());

/// Locks [`OUTPUT`], recovering it if a panicking writer poisoned it.
fn lock_output() -> std::sync::MutexGuard<'static, ()> {
    OUTPUT.lock().unwrap_or_else(PoisonError::into_inner)
}

/// Marks the process interrupted, waits for any record in flight,
/// writes the `interrupted` error record to `stderr` unless another
/// thread already did, and exits 130 still holding [`OUTPUT`], so no
/// other thread can start a record after it.
pub fn interrupt_exit(stderr: &mut dyn Write) -> ! {
    INTERRUPTED.store(true, Ordering::SeqCst);

    let _guard = lock_output();

    if !REPORTED.swap(true, Ordering::SeqCst) {
        write_record(stderr, "interrupted", "interrupted", 0);
    }

    process::exit(EXIT_INTERRUPTED)
}

/// Exits with `code`, or through [`interrupt_exit`] once SIGINT has
/// been seen. Holds [`OUTPUT`] while exiting, so a handler that is
/// writing the record finishes it first and one that starts later
/// never writes.
pub fn exit(code: i32, stderr: &mut dyn Write) -> ! {
    let guard = lock_output();

    if INTERRUPTED.load(Ordering::SeqCst) {
        drop(guard);
        interrupt_exit(stderr);
    }

    process::exit(code)
}

// ── Help text ───────────────────────────────────────────────────────────────

/// Top-level `after_help`: the input note, the stdout contract, and
/// the exit-code table, verbatim from the Go tool.
const TOP_AFTER_HELP: &str = "\
Input:
  --input <path|->      read JSON from a file, or -
                        for stdin.

stdout is data only: one JSON document, compact
unless --pretty. Failures print one JSON object as
the last stderr line.

Exit codes:
  0    success
  1    unexpected failure
  2    usage: bad flags, missing argument, bad input
  3    authentication failed or no API key
  5    request rejected (422 or other 4xx)
  6    rate limited after retries
  7    upstream error (5xx after retries, or an invalid response)
  8    network failure or timeout
  130  interrupted (SIGINT)";

/// `raw --help` output sentence and example.
const RAW_AFTER_HELP: &str = "\
Output: the System One response as one JSON object.

Example:
  printf '%s' '{\"state\":\"Payouts have failed for 3 days.\",
    \"questions\":{\"urgent\":{\"type\":\"noul\",
    \"instructions\":\"Does this convey urgency?\"}}}' |
    overseer-judge raw --input - --pretty";

/// `session --help` output sentence and example.
const SESSION_AFTER_HELP: &str = "\
Output: the verdict as one JSON object: state, confidence, \
probabilities, coherent, input_line, activity_hint, model, usage.

Example:
  # ANSI input is preferred. Only ANSI lets the tool
  # drop an agent's greyed-out prompt suggestion,
  # which in plain text is indistinguishable from
  # text the user typed and has not submitted.
  herdr agent read <pane> --lines 60 \\
    --source recent-unwrapped --format ansi |
    overseer-judge session --input - --agent claude

  tmux capture-pane -p -e -J -S -60 -t <pane> |
    overseer-judge session --input - --agent pi";

/// `task --help` output sentence and example.
const TASK_AFTER_HELP: &str = "\
Output: the lint report as one JSON object: file, static, judgments, \
acceptance, model, usage.

Example:
  overseer-judge task --pretty .overseer/tasks/003-tasklint.md";

/// `review --help` output sentence and example.
const REVIEW_AFTER_HELP: &str = "\
Output: JSON Lines, one object per item: id, severity, style_only, \
responses, model, usage, warnings. --pretty is rejected: a record stays \
on one line. A file with content and no items is a usage error (exit \
2).

Example:
  overseer-judge review .overseer/review/round-4.md |
    jq -c 'select(.style_only > 0.5)'";

/// Flags whose next token is their value when written without `=`.
const VALUE_FLAGS: [&str; 5] =
    ["--input", "--model", "--timeout", "--log-level", "--agent"];

// ── Arguments ───────────────────────────────────────────────────────────────

/// The parsed command line.
#[derive(Debug, Parser)]
#[command(
    name = "overseer-judge",
    version,
    about = "Turns overseer judgments into typed JSON verdicts via \
             TypeSafe's System One (Jev) API.",
    long_about = None,
    after_help = TOP_AFTER_HELP,
    subcommand_required = true,
    disable_help_subcommand = true,
    propagate_version = true,
    args_override_self = true
)]
struct Cli {
    /// Flags every verb accepts.
    #[command(flatten)]
    globals: Globals,
    /// The verb to run.
    #[command(subcommand)]
    verb: Verb,
}

/// The global flags, accepted before or after the verb.
#[derive(Debug, Args)]
struct Globals {
    /// Indent JSON output
    #[arg(
        long,
        global = true,
        action = ArgAction::Set,
        num_args = 0..=1,
        default_value = "false",
        default_missing_value = "true",
        require_equals = true,
        hide_default_value = true,
        hide_possible_values = true,
        value_parser = go_bool(),
    )]
    pretty: bool,
    /// Print the request that would be sent and exit 0
    #[arg(
        long,
        global = true,
        action = ArgAction::Set,
        num_args = 0..=1,
        default_value = "false",
        default_missing_value = "true",
        require_equals = true,
        hide_default_value = true,
        hide_possible_values = true,
        value_parser = go_bool(),
    )]
    dry_run: bool,
    /// Jev model (default from TYPESAFE_DEFAULT_MODEL)
    #[arg(
        long,
        global = true,
        value_name = "name",
        allow_hyphen_values = true
    )]
    model: Option<String>,
    /// Timeout for each HTTP attempt; 0 or negative disables it
    #[arg(
        long,
        global = true,
        value_name = "duration",
        default_value = "10s",
        allow_hyphen_values = true,
        value_parser = parse_go_duration,
    )]
    timeout: ::std::option::Option<Duration>,
    /// Stderr diagnostics
    #[arg(
        long,
        global = true,
        value_name = "level",
        value_enum,
        default_value_t = LogLevel::Warn
    )]
    log_level: LogLevel,
}

/// The `--log-level` values.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum LogLevel {
    /// Everything, including per-request byte counts.
    Debug,
    /// Progress.
    Info,
    /// Recoverable oddities.
    Warn,
    /// Failures only.
    Error,
}

impl From<LogLevel> for Level {
    /// Maps a `--log-level` value onto its tracing level.
    fn from(level: LogLevel) -> Self {
        match level {
            LogLevel::Debug => Self::DEBUG,
            LogLevel::Info => Self::INFO,
            LogLevel::Warn => Self::WARN,
            LogLevel::Error => Self::ERROR,
        }
    }
}

impl ValueEnum for LogLevel {
    /// Every level, most verbose first.
    fn value_variants<'a>() -> &'a [Self] {
        &[Self::Debug, Self::Info, Self::Warn, Self::Error]
    }

    /// The level's flag value. No per-value help, so `-h` and
    /// `--help` print the same text.
    fn to_possible_value(&self) -> Option<PossibleValue> {
        Some(PossibleValue::new(match self {
            Self::Debug => "debug",
            Self::Info => "info",
            Self::Warn => "warn",
            Self::Error => "error",
        }))
    }
}

impl ValueEnum for AgentKind {
    /// Every kind, in help order.
    fn value_variants<'a>() -> &'a [Self] {
        &[
            Self::Claude,
            Self::Codex,
            Self::Pi,
            Self::Opencode,
            Self::Unknown,
        ]
    }

    /// The kind's wire name.
    fn to_possible_value(&self) -> Option<PossibleValue> {
        Some(PossibleValue::new(self.as_str()))
    }
}

/// The four verbs.
#[derive(Debug, Subcommand)]
enum Verb {
    /// Send an arbitrary Jev request read from --input.
    #[command(
        about = "send an arbitrary Jev request read from --input",
        long_about = None,
        after_help = RAW_AFTER_HELP,
        args_override_self = true
    )]
    Raw {
        /// Request JSON to send; - reads stdin
        #[arg(long, value_name = "path|-", allow_hyphen_values = true)]
        input: String,
    },
    /// Classify an agent pane's transcript tail read from --input.
    #[command(
        about = "classify an agent pane's transcript tail read from --input",
        long_about = None,
        after_help = SESSION_AFTER_HELP,
        args_override_self = true
    )]
    Session {
        /// Transcript tail; - reads stdin
        #[arg(long, value_name = "path|-", allow_hyphen_values = true)]
        input: String,
        /// Agent kind
        #[arg(long, value_name = "kind", default_value = "unknown")]
        agent: AgentKind,
    },
    /// Lint an overseer task file for spec defects.
    #[command(
        about = "lint an overseer task file for spec defects",
        long_about = None,
        after_help = TASK_AFTER_HELP,
        args_override_self = true
    )]
    Task {
        /// Task file; - reads stdin
        #[arg(value_name = "path|-")]
        path: String,
    },
    /// Type each item and response in an overseer review round file.
    #[command(
        about = "type each item and response in an overseer review round \
                 file",
        long_about = None,
        after_help = REVIEW_AFTER_HELP,
        args_override_self = true
    )]
    Review {
        /// Review round file; - reads stdin
        #[arg(value_name = "path|-")]
        path: String,
    },
}

/// Every spelling Go's `strconv.ParseBool` accepts, true ones first.
const GO_BOOLS: [&str; 12] = [
    "1", "t", "T", "TRUE", "true", "True", "0", "f", "F", "FALSE", "false",
    "False",
];

/// Returns the `--pretty` / `--dry-run` value parser: exactly
/// [`GO_BOOLS`], mapped onto `bool`.
fn go_bool() -> impl TypedValueParser<Value = bool> {
    PossibleValuesParser::new(GO_BOOLS)
        .map(|value| GO_BOOLS[..6].contains(&value.as_str()))
}

/// Parses a Go `time.Duration` string such as `10s`, `1h30m`, `.5s`,
/// or `0`.
///
/// Truncates the total to whole nanoseconds, then returns `Ok(None)`
/// for a zero or negative result, meaning "no timeout", and
/// `Ok(Some(d))` for a positive one.
///
/// # Errors
///
/// Returns a message when `s` is not a Go duration: empty, a missing
/// or unknown unit (`1`, `1d`), a sign anywhere but the front
/// (`1h-30m`), or a total above `u64::MAX` nanoseconds.
pub fn parse_go_duration(s: &str) -> Result<Option<Duration>, String> {
    /// Units by suffix, two-letter ones first so `ms` beats `m`.
    const UNITS: [(&str, f64); 8] = [
        ("ns", 1.0),
        ("us", 1e3),
        ("µs", 1e3),
        ("μs", 1e3),
        ("ms", 1e6),
        ("s", 1e9),
        ("m", 60e9),
        ("h", 3600e9),
    ];

    let invalid = || format!("invalid duration {s:?}");
    let (negative, mut rest) = match s.strip_prefix('-') {
        Some(rest) => (true, rest),
        None => (false, s.strip_prefix('+').unwrap_or(s)),
    };

    if rest == "0" {
        return Ok(None);
    }

    if rest.is_empty() {
        return Err(invalid());
    }

    let mut nanos = 0.0_f64;

    while !rest.is_empty() {
        let end = rest
            .find(|c: char| !c.is_ascii_digit() && c != '.')
            .unwrap_or(rest.len());
        let (number, tail) = rest.split_at(end);
        // Rust's f64 grammar takes `1.` and `.5` but not `.` or `1.2.3`,
        // which is exactly Go's `\d+(\.\d*)?|\.\d+` over digits and dots.
        let value: f64 = number.parse().map_err(|_| invalid())?;
        let (unit, scale) = UNITS
            .iter()
            .find(|(unit, _)| tail.starts_with(unit))
            .ok_or_else(invalid)?;

        nanos += value * scale;
        rest = &tail[unit.len()..];
    }

    if nanos >= u64::MAX as f64 {
        return Err(format!("invalid duration {s:?}: overflows"));
    }

    let nanos = nanos as u64;

    if negative || nanos == 0 {
        return Ok(None);
    }

    Ok(Some(Duration::from_nanos(nanos)))
}

/// Rewrites Go-style single-dash long flags (`-input`, `-pretty=false`)
/// to their double-dash form, leaving flag values, a lone `-`, and
/// everything after `--` untouched.
fn rewrite_go_flags(args: Vec<OsString>) -> Vec<OsString> {
    let mut out = Vec::with_capacity(args.len());
    let mut args = args.into_iter();

    while let Some(arg) = args.next() {
        if arg == "--" {
            out.push(arg);
            out.extend(args);

            break;
        }

        let Some(text) = arg.to_str() else {
            out.push(arg);

            continue;
        };

        let flag = if is_go_long_flag(text) {
            format!("-{text}")
        } else {
            text.to_owned()
        };
        let takes_value = !flag.contains('=') && VALUE_FLAGS.contains(&&*flag);

        out.push(flag.into());

        if takes_value && let Some(value) = args.next() {
            out.push(value);
        }
    }

    out
}

/// Reports whether `token` is a single-dash long flag:
/// `^-[A-Za-z][A-Za-z0-9-]*(=.*)?$`, longer than two characters.
fn is_go_long_flag(token: &str) -> bool {
    let Some(rest) = token.strip_prefix('-') else {
        return false;
    };
    let name = rest.split_once('=').map_or(rest, |(name, _)| name);

    token.len() > 2
        && name.starts_with(|c: char| c.is_ascii_alphabetic())
        && name.chars().all(|c| c.is_ascii_alphanumeric() || c == '-')
}

// ── Errors ──────────────────────────────────────────────────────────────────

/// Why a verb failed; each variant maps onto an exit code.
#[derive(Debug, thiserror::Error)]
enum Error {
    /// Bad flags, a missing argument, or bad input.
    #[error("{0}")]
    Usage(String),
    /// No API key is configured.
    #[error(transparent)]
    NoKey(#[from] config::NoKey),
    /// The System One call failed.
    #[error(transparent)]
    Jev(#[from] jev::Error),
    /// Output could not be encoded.
    #[error("encode output: {0}")]
    Encode(#[from] serde_json::Error),
    /// Output could not be written.
    #[error("write output: {0}")]
    Output(#[from] io::Error),
}

impl Error {
    /// Returns the record type, HTTP status (0 when none), and exit
    /// code for this error.
    fn classify(&self) -> (&'static str, u16, i32) {
        match self {
            Self::Usage(_) => ("usage", 0, EXIT_USAGE),
            Self::NoKey(_) => ("auth", 0, EXIT_AUTH),
            Self::Jev(jev::Error::Auth { status }) => {
                ("auth", *status, EXIT_AUTH)
            }
            Self::Jev(jev::Error::Request { status, .. }) => {
                ("request", *status, EXIT_REQUEST)
            }
            Self::Jev(jev::Error::RateLimit { status }) => {
                ("rate_limit", *status, EXIT_RATE_LIMIT)
            }
            Self::Jev(jev::Error::Server { status, .. }) => {
                ("server", *status, EXIT_UPSTREAM)
            }
            Self::Jev(jev::Error::Response(_)) => {
                ("response", 0, EXIT_UPSTREAM)
            }
            Self::Jev(jev::Error::Network(_)) => ("network", 0, EXIT_NETWORK),
            Self::Encode(_) | Self::Output(_) => ("unknown", 0, EXIT_UNKNOWN),
        }
    }
}

/// The JSON object written as the last stderr line on failure.
#[derive(Debug, Serialize)]
struct ErrorRecord<'a> {
    /// The failure.
    error: ErrorBody<'a>,
}

/// One failure in an [`ErrorRecord`].
#[derive(Debug, Serialize)]
struct ErrorBody<'a> {
    /// Machine-readable category, such as `usage` or `network`.
    r#type: &'a str,
    /// Human-readable description, API key redacted.
    message: &'a str,
    /// HTTP status, omitted when there is none.
    #[serde(skip_serializing_if = "Option::is_none")]
    status: Option<u16>,
}

/// Writes the error record for `kind`, `message`, and `status` (0 for
/// none) as one line to `stderr`.
fn write_record(
    stderr: &mut dyn Write,
    kind: &str,
    message: &str,
    status: u16,
) {
    let record = ErrorRecord {
        error: ErrorBody {
            r#type: kind,
            message,
            status: (status != 0).then_some(status),
        },
    };
    let line = serde_json::to_string(&record).unwrap_or_else(|err| {
        unreachable!("a struct of strings and a u16 serializes: {err}")
    });

    // Nothing is left to report a failure to: stderr is the last resort.
    let _ = writeln!(stderr, "{line}").and_then(|()| stderr.flush());
}

// ── Output ──────────────────────────────────────────────────────────────────

/// What `--dry-run` prints instead of sending a request.
#[derive(Debug, Serialize)]
struct DryRun<'a> {
    /// `POST`, or empty when no request would be sent.
    method: &'a str,
    /// The endpoint path, or empty when no request would be sent.
    path: &'a str,
    /// The request body, or `null` when no request would be sent.
    body: Option<Box<RawValue>>,
    /// The static findings of a task file too broken to judge.
    #[serde(skip_serializing_if = "Option::is_none")]
    r#static: Option<Vec<tasklint::Finding>>,
    /// The review item's warnings, omitted when empty.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    warnings: Vec<review::Warning>,
}

impl DryRun<'_> {
    /// Returns the record for sending `body` to the endpoint.
    fn post(body: Box<RawValue>) -> Self {
        Self {
            method: "POST",
            path: jev::PATH,
            body: Some(body),
            r#static: None,
            warnings: Vec::new(),
        }
    }

    /// Returns the record for `req` with `model` filled in.
    fn request(mut req: jev::Request, model: &str) -> Result<Self, Error> {
        req.model = Some(model.to_owned());

        Ok(Self::post(serde_json::value::to_raw_value(&req)?))
    }
}

/// Writes `value` to `stdout` as one JSON document plus a newline,
/// indented when `pretty`, and flushes it, all under [`OUTPUT`].
///
/// # Errors
///
/// [`Error::Output`] without writing once SIGINT has been seen, so
/// [`run`] reports the interrupt instead of starting a record.
fn write_json(
    stdout: &mut dyn Write,
    value: &impl Serialize,
    pretty: bool,
) -> Result<(), Error> {
    let text = format_json(&serde_json::to_string(value)?, pretty);
    let _guard = lock_output();

    if INTERRUPTED.load(Ordering::SeqCst) {
        return Err(io::Error::from(io::ErrorKind::Interrupted).into());
    }

    writeln!(stdout, "{text}")?;
    stdout.flush()?;

    Ok(())
}

/// Re-emits valid JSON `json` without insignificant whitespace, or
/// with two-space indentation when `pretty`.
///
/// serde_json writes a [`RawValue`] verbatim, so this is what compacts
/// or indents the `raw` verb's pass-through input like everything else.
fn format_json(json: &str, pretty: bool) -> String {
    let mut out = String::with_capacity(json.len());
    let mut chars = json.chars().peekable();
    let mut depth = 0_usize;
    let mut in_string = false;
    let mut escaped = false;

    while let Some(c) = chars.next() {
        if in_string {
            out.push(c);

            if escaped {
                escaped = false;
            } else if c == '\\' {
                escaped = true;
            } else if c == '"' {
                in_string = false;
            }

            continue;
        }

        match c {
            ' ' | '\t' | '\n' | '\r' => {}
            '"' => {
                in_string = true;
                out.push(c);
            }
            '{' | '[' if pretty => {
                out.push(c);

                while chars.next_if(char::is_ascii_whitespace).is_some() {}

                let close = if c == '{' { '}' } else { ']' };

                if let Some(close) = chars.next_if_eq(&close) {
                    out.push(close);

                    continue;
                }

                depth += 1;
                newline(&mut out, depth);
            }
            '}' | ']' if pretty => {
                depth = depth.saturating_sub(1);
                newline(&mut out, depth);
                out.push(c);
            }
            ',' if pretty => {
                out.push(c);
                newline(&mut out, depth);
            }
            ':' if pretty => out.push_str(": "),
            _ => out.push(c),
        }
    }

    out
}

/// Starts a new line indented to `depth`.
fn newline(out: &mut String, depth: usize) {
    out.push('\n');
    out.push_str(&"  ".repeat(depth));
}

// ── Run ─────────────────────────────────────────────────────────────────────

/// Parses `args` (without argv\[0\]), runs the verb, and returns the
/// exit code.
///
/// Help and version go to `stdout` with exit 0. Every failure writes
/// one JSON error record as the last `stderr` line. Exits the process
/// only through [`interrupt_exit`], once SIGINT has been seen, and
/// never holds [`OUTPUT`] or a stdout or stderr lock across a blocking
/// read or request.
pub fn run(
    args: Vec<OsString>,
    stdin: &mut dyn Read,
    stdout: &mut dyn Write,
    stderr: &mut dyn Write,
) -> i32 {
    let argv = iter::once(OsString::from("overseer-judge"))
        .chain(rewrite_go_flags(args));
    let cli = match Cli::try_parse_from(argv) {
        Ok(cli) => cli,
        Err(err) => return report_clap(&err, stdout, stderr),
    };

    let subscriber = tracing_subscriber::fmt()
        .with_max_level(Level::from(cli.globals.log_level))
        .with_writer(io::stderr)
        .finish();
    let mut session = Session {
        globals: &cli.globals,
        config: Config::from_env(),
        key: None,
    };
    let result = tracing::subscriber::with_default(subscriber, || {
        session.dispatch(&cli.verb, stdin, stdout)
    });

    let Err(err) = result else {
        return EXIT_OK;
    };

    let (kind, status, code) = err.classify();
    let mut message = err.to_string();

    if let Some(key) = session.key.as_deref().filter(|key| !key.is_empty()) {
        message = message.replace(key, "[redacted]");
    }

    let guard = lock_output();

    if INTERRUPTED.load(Ordering::SeqCst) {
        drop(guard);
        interrupt_exit(stderr);
    }

    write_record(stderr, kind, &message, status);

    code
}

/// Handles a clap parse result that is not a parsed command line:
/// help and version to `stdout` (exit 0), anything else to `stderr`
/// followed by a usage record (exit 2).
fn report_clap(
    err: &clap::Error,
    stdout: &mut dyn Write,
    stderr: &mut dyn Write,
) -> i32 {
    let text = err.render().to_string();

    if matches!(
        err.kind(),
        ErrorKind::DisplayHelp | ErrorKind::DisplayVersion
    ) {
        let guard = lock_output();
        let written = write!(stdout, "{text}").and_then(|()| stdout.flush());

        if INTERRUPTED.load(Ordering::SeqCst) {
            drop(guard);
            interrupt_exit(stderr);
        }

        return match written {
            Ok(()) => EXIT_OK,
            Err(err) => {
                write_record(
                    stderr,
                    "unknown",
                    &format!("write output: {err}"),
                    0,
                );

                EXIT_UNKNOWN
            }
        };
    }

    let message =
        if err.kind() == ErrorKind::DisplayHelpOnMissingArgumentOrSubcommand {
            "no verb given"
        } else {
            let line = text.lines().next().unwrap_or_default();

            line.strip_prefix("error: ").unwrap_or(line)
        };

    let guard = lock_output();

    if INTERRUPTED.load(Ordering::SeqCst) {
        drop(guard);
        interrupt_exit(stderr);
    }

    // The record below is the error report; a failed write of the
    // human-readable text before it changes nothing.
    let _ = write!(stderr, "{text}");
    write_record(stderr, "usage", message, 0);

    EXIT_USAGE
}

/// One invocation's settings, plus the API key once it is resolved.
struct Session<'a> {
    /// The parsed global flags.
    globals: &'a Globals,
    /// Settings from the environment.
    config: Config,
    /// The resolved API key, kept so error messages can redact it.
    key: Option<String>,
}

impl Session<'_> {
    /// Returns `--model` when given and non-empty, else the configured
    /// default.
    fn model(&self) -> &str {
        self.globals
            .model
            .as_deref()
            .filter(|model| !model.is_empty())
            .unwrap_or(&self.config.model)
    }

    /// Resolves the API key and returns a client using it.
    ///
    /// # Errors
    ///
    /// [`Error::NoKey`] when no key is configured.
    fn client(&mut self) -> Result<jev::Client, Error> {
        let env_key = env::var("TYPESAFE_API_KEY").ok();
        let key =
            config::key(env_key.as_deref(), &config::default_key_file())?;
        let client = jev::Client::new(
            &self.config.base_url,
            &key,
            self.model(),
            self.globals.timeout,
        );

        self.key = Some(key);

        Ok(client)
    }

    /// Runs `verb`.
    ///
    /// # Errors
    ///
    /// Whatever the verb fails with.
    fn dispatch(
        &mut self,
        verb: &Verb,
        stdin: &mut dyn Read,
        stdout: &mut dyn Write,
    ) -> Result<(), Error> {
        match verb {
            Verb::Raw { input } => self.raw(input, stdin, stdout),
            Verb::Session { input, agent } => {
                self.session(input, *agent, stdin, stdout)
            }
            Verb::Task { path } => self.task(path, stdin, stdout),
            Verb::Review { path } => self.review(path, stdin, stdout),
        }
    }

    /// Sends the request read from `input` verbatim, filling in the
    /// model when absent, or prints it with `--dry-run`.
    ///
    /// # Errors
    ///
    /// [`Error::Usage`] for unreadable or invalid input, else the
    /// client's errors.
    fn raw(
        &mut self,
        input: &str,
        stdin: &mut dyn Read,
        stdout: &mut dyn Write,
    ) -> Result<(), Error> {
        let bytes = read_input(input, stdin, "open --input")?;
        let body = raw_body(&bytes, self.model())?;

        if self.globals.dry_run {
            return write_json(
                stdout,
                &DryRun::post(body),
                self.globals.pretty,
            );
        }

        let client = self.client()?;

        debug!(bytes = body.get().len(), "sending raw request");

        let response = client.ask_raw(body.get().as_bytes())?;

        write_json(stdout, &response, self.globals.pretty)
    }

    /// Classifies the transcript tail read from `input`, or prints the
    /// request with `--dry-run`.
    ///
    /// # Errors
    ///
    /// [`Error::Usage`] for unreadable input, else the client's errors.
    fn session(
        &mut self,
        input: &str,
        agent: AgentKind,
        stdin: &mut dyn Read,
        stdout: &mut dyn Write,
    ) -> Result<(), Error> {
        let bytes = read_input(input, stdin, "open --input")?;
        let tail = String::from_utf8_lossy(&bytes);

        if self.globals.dry_run {
            let record =
                DryRun::request(session::request(agent, &tail), self.model())?;

            return write_json(stdout, &record, self.globals.pretty);
        }

        let client = self.client()?;

        debug!(
            bytes = bytes.len(),
            agent = agent.as_str(),
            "classifying tail"
        );

        let verdict = session::judge(&client, agent, &tail)?;

        write_json(stdout, &verdict, self.globals.pretty)
    }

    /// Lints the task file at `path`, or prints the request (or, for a
    /// file too broken to judge, the static findings) with `--dry-run`.
    ///
    /// # Errors
    ///
    /// [`Error::Usage`] for an unreadable file, [`Error::NoKey`] even
    /// when no request would be sent, else the client's errors.
    fn task(
        &mut self,
        path: &str,
        stdin: &mut dyn Read,
        stdout: &mut dyn Write,
    ) -> Result<(), Error> {
        let bytes = read_input(path, stdin, &format!("read {path}"))?;
        let text = String::from_utf8_lossy(&bytes);

        if self.globals.dry_run {
            let doc = tasklint::parse(&text);
            let record = if doc.judgeable() {
                DryRun::request(tasklint::request(&doc), self.model())?
            } else {
                DryRun {
                    method: "",
                    path: "",
                    body: None,
                    r#static: Some(tasklint::static_checks(&doc)),
                    warnings: Vec::new(),
                }
            };

            return write_json(stdout, &record, self.globals.pretty);
        }

        let client = self.client()?;

        debug!(bytes = bytes.len(), "linting task file");

        let report = tasklint::judge(&client, path, &text)?;

        write_json(stdout, &report, self.globals.pretty)
    }

    /// Types each item of the review round at `path`, one JSON line per
    /// item as its answer arrives, or prints one request per line with
    /// `--dry-run`.
    ///
    /// # Errors
    ///
    /// [`Error::Usage`] for `--pretty`, an unreadable file, or a file
    /// with content and no items, else the first client error; lines
    /// already written stay written.
    fn review(
        &mut self,
        path: &str,
        stdin: &mut dyn Read,
        stdout: &mut dyn Write,
    ) -> Result<(), Error> {
        if self.globals.pretty {
            return Err(Error::Usage(
                "review does not accept --pretty: its output is JSON Lines, \
                 one item per line"
                    .into(),
            ));
        }

        let bytes = read_input(path, stdin, &format!("read {path}"))?;
        let text = String::from_utf8_lossy(&bytes);
        let items = review::parse(&text);

        debug!(items = items.len(), "parsed review round");

        // A round with no items needs no request, so no key either.
        if items.is_empty() {
            if text.trim().is_empty() {
                return Ok(());
            }
            return Err(Error::Usage(format!(
                "no review items in {path}: expected \"### R<n>-<nn>: \
                 title\" headers"
            )));
        }

        for item in &items {
            for warning in &item.warnings {
                warn!(
                    id = %item.id,
                    warning = warning.as_str(),
                    "review item did not fully parse"
                );
            }
        }

        if self.globals.dry_run {
            for item in &items {
                let mut record =
                    DryRun::request(review::request(item), self.model())?;
                record.warnings.clone_from(&item.warnings);

                write_json(stdout, &record, false)?;
            }

            return Ok(());
        }

        let client = self.client()?;

        for item in &items {
            write_json(stdout, &review::judge_item(&client, item)?, false)?;
        }

        Ok(())
    }
}

/// Reads all of `path`, where `-` means `stdin`.
///
/// # Errors
///
/// [`Error::Usage`] prefixed with `what` when the read fails.
fn read_input(
    path: &str,
    stdin: &mut dyn Read,
    what: &str,
) -> Result<Vec<u8>, Error> {
    let result = if path == "-" {
        let mut bytes = Vec::new();

        stdin.read_to_end(&mut bytes).map(|_| bytes)
    } else {
        fs::read(path)
    };

    result.map_err(|err| Error::Usage(format!("{what}: {err}")))
}

/// Parses `bytes` as exactly one JSON object holding `state` and
/// `questions`, inserts `model` when the object names none, and returns
/// the compact body with nested values kept verbatim.
///
/// # Errors
///
/// [`Error::Usage`] when the input is not one such object.
fn raw_body(bytes: &[u8], model: &str) -> Result<Box<RawValue>, Error> {
    /// The input object; nested values stay byte-verbatim.
    type Object = BTreeMap<String, Box<RawValue>>;

    let usage = |message: String| Error::Usage(message);
    let mut values = serde_json::Deserializer::from_slice(bytes)
        .into_iter::<Option<Object>>();

    let mut object = match values.next() {
        None => {
            return Err(usage("--input must be one JSON object: EOF".into()));
        }
        Some(Err(err)) => {
            return Err(usage(format!(
                "--input must be one JSON object: {err}"
            )));
        }
        Some(Ok(None)) => {
            return Err(usage(
                "--input must be a JSON object, got null".into(),
            ));
        }
        Some(Ok(Some(object))) => object,
    };

    if values.next().is_some() {
        return Err(usage("--input must hold exactly one JSON object".into()));
    }

    for key in ["state", "questions"] {
        if !object.contains_key(key) {
            return Err(usage(format!("--input is missing {key:?}")));
        }
    }

    if !object.contains_key("model") {
        object.insert("model".into(), serde_json::value::to_raw_value(model)?);
    }

    let compact = format_json(&serde_json::to_string(&object)?, false);

    Ok(RawValue::from_string(compact)?)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn should_parse_go_durations() {
        let accepted = [
            ("10s", Duration::from_secs(10)),
            ("500ms", Duration::from_millis(500)),
            ("1h30m", Duration::from_secs(5400)),
            (".5s", Duration::from_millis(500)),
            ("1ns", Duration::from_nanos(1)),
            ("1.5s", Duration::from_millis(1500)),
            ("1µs", Duration::from_micros(1)),
            ("1μs", Duration::from_micros(1)),
            ("+2us", Duration::from_micros(2)),
            ("3ns", Duration::from_nanos(3)),
        ];
        for (input, want) in accepted {
            assert_eq!(parse_go_duration(input), Ok(Some(want)), "{input}");
        }

        for input in ["0", "+0", "-0", "-1s", "0s", ".5ns", "0.999ns"] {
            assert_eq!(parse_go_duration(input), Ok(None), "{input}");
        }

        for input in ["1d", "", "abc", "1", "1h-30m", "-", ".s", "1e3s"] {
            assert!(parse_go_duration(input).is_err(), "{input} accepted");
        }
    }

    #[test]
    fn should_rewrite_only_single_dash_long_flags() {
        let args = [
            "raw",
            "-input",
            "-",
            "-pretty=false",
            "-h",
            "--model",
            "-preview",
            "-timeout",
            "-1s",
            "--",
            "-weird",
        ];

        let got = rewrite_go_flags(args.iter().map(OsString::from).collect());

        let want = [
            "raw",
            "--input",
            "-",
            "--pretty=false",
            "-h",
            "--model",
            "-preview",
            "--timeout",
            "-1s",
            "--",
            "-weird",
        ];
        assert_eq!(got, want.map(OsString::from));
    }

    #[test]
    fn should_format_json_compact_and_pretty() {
        let json = r#"{ "a" : [1, {}, [ ] ], "b":"x, \" {y}" }"#;

        assert_eq!(
            format_json(json, false),
            r#"{"a":[1,{},[]],"b":"x, \" {y}"}"#
        );
        assert_eq!(
            format_json(json, true),
            "{\n  \"a\": [\n    1,\n    {},\n    []\n  ],\n  \"b\": \"x, \\\" {y}\"\n}"
        );
    }

    #[test]
    fn should_reject_raw_input_that_is_not_one_object() {
        let cases = [
            ("", "--input must be one JSON object: EOF"),
            ("null", "--input must be a JSON object, got null"),
            ("{} {}", "--input must hold exactly one JSON object"),
            ("{}]", "--input must hold exactly one JSON object"),
            (r#"{"state":1}"#, "--input is missing \"questions\""),
        ];

        for (input, want) in cases {
            let err = raw_body(input.as_bytes(), "m").unwrap_err();

            assert_eq!(err.to_string(), want, "{input}");
        }
        assert!(
            raw_body(b"[1]", "m")
                .unwrap_err()
                .to_string()
                .starts_with("--input must be one JSON object: ")
        );
    }

    #[test]
    fn should_keep_raw_model_and_nested_order() {
        let body = raw_body(
            br#"{"state":{"z":1, "a":2},"questions":{},"model":"x"}"#,
            "m",
        )
        .unwrap();

        assert_eq!(
            body.get(),
            r#"{"model":"x","questions":{},"state":{"z":1,"a":2}}"#
        );
    }
}
