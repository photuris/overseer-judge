//! Integration tests of the `overseer-judge` binary against a mock
//! System One server.
//!
//! Every invocation gets `XDG_CONFIG_HOME` pointed at an empty temp
//! dir, `TYPESAFE_BASE_URL` at the mock, and `TYPESAFE_API_KEY` set or
//! cleared per case, so the developer's own key file never leaks in.

// Clippy exempts `#[test]` bodies but not their setup helpers, which
// panic on setup failure for the same reason the tests do.
#![expect(clippy::unwrap_used, reason = "test helpers")]

#[cfg(unix)]
use std::thread;
use std::{
    fs,
    io::{BufRead, BufReader},
    net::TcpListener,
    path::{Path, PathBuf},
    process::{self, Output, Stdio},
    time::{Duration, Instant},
};

use assert_cmd::Command;
use httpmock::prelude::*;
use serde_json::Value;
use tempfile::TempDir;

/// The binary under test.
const BIN: &str = env!("CARGO_BIN_EXE_overseer-judge");

/// The API key sent by tests that need one.
const KEY: &str = "sekret-key-123";

/// The model every invocation gets from `TYPESAFE_DEFAULT_MODEL`.
const ENV_MODEL: &str = "jev-env";

/// A minimal valid `raw` request.
const RAW_INPUT: &str = r#"{"state":"Payouts have failed.","questions":{"a":{"type":"noul","instructions":"Urgent?"}}}"#;

/// A response with a full noul answer and one carrying only `type`.
const RAW_RESPONSE: &str = r#"{"model":"jev-1","answers":{
    "a":{"type":"noul","noul":0.25},"b":{"type":"choice"}},
    "usage":{"input_tokens":12,"output_tokens":3}}"#;

/// A valid `session` answer pair.
const SESSION_RESPONSE: &str = r#"{"model":"jev-1","answers":{
    "state":{"type":"choice","choice":"idle","confidence":0.91,
        "probabilities":{"idle":0.91,"working":0.09}},
    "coherent":{"type":"noul","noul":0.97}},
    "usage":{"input_tokens":1200,"output_tokens":8}}"#;

/// A review round with two items of one response each.
const REVIEW_ROUND: &str = "# Review round 1

### R1-01: First finding
- severity: medium
The first body.
- response: fixed: done

### R1-02: Second finding
- severity: low
The second body.
- response: fixed: also done
";

/// A task file with only a title and a status: not judgeable.
const STATIC_ONLY_TASK: &str = "# 001 empty\nStatus: ready\n";

/// An isolated environment: an empty config dir and scratch files.
struct Sandbox {
    /// Stands in for `XDG_CONFIG_HOME`; holds no key file.
    dir: TempDir,
}

impl Sandbox {
    /// Returns a fresh sandbox.
    fn new() -> Self {
        Self {
            dir: tempfile::tempdir().unwrap(),
        }
    }

    /// Writes `contents` to `name` in the sandbox and returns its path.
    fn file(&self, name: &str, contents: &str) -> PathBuf {
        let path = self.dir.path().join(name);
        fs::write(&path, contents).unwrap();

        path
    }

    /// Returns a command for the binary with `args`, talking to
    /// `base_url`, with `key` as `TYPESAFE_API_KEY` or none.
    fn command(
        &self,
        base_url: &str,
        key: Option<&str>,
        args: &[&str],
    ) -> process::Command {
        let mut cmd = process::Command::new(BIN);
        cmd.args(args)
            .env("XDG_CONFIG_HOME", self.dir.path())
            .env("TYPESAFE_BASE_URL", base_url)
            .env("TYPESAFE_DEFAULT_MODEL", ENV_MODEL)
            .env_remove("TYPESAFE_API_KEY");
        if let Some(key) = key {
            cmd.env("TYPESAFE_API_KEY", key);
        }

        cmd
    }

    /// Runs the binary with `args` and `stdin` to completion.
    fn run(
        &self,
        base_url: &str,
        key: Option<&str>,
        args: &[&str],
        stdin: &str,
    ) -> Output {
        Command::from_std(self.command(base_url, key, args))
            .write_stdin(stdin)
            .output()
            .unwrap()
    }
}

/// Serves `status` with `body` (after `delay`) to every POST on
/// `server`.
fn serve<'a>(
    server: &'a MockServer,
    status: u16,
    body: &str,
    delay: Duration,
) -> httpmock::Mock<'a> {
    server.mock(|when, then| {
        when.method(POST).path("/v1/systemone");
        then.status(status).body(body).delay(delay);
    })
}

/// Returns `out`'s stdout as text.
fn stdout(out: &Output) -> String {
    String::from_utf8(out.stdout.clone()).unwrap()
}

/// Returns `out`'s stderr as text.
fn stderr(out: &Output) -> String {
    String::from_utf8(out.stderr.clone()).unwrap()
}

/// Returns the single JSON document on `out`'s stdout.
fn stdout_json(out: &Output) -> Value {
    serde_json::from_slice(&out.stdout).unwrap()
}

/// Asserts `out` failed with `code`, that exactly one stderr line is
/// an error record, that it is the last line, and that its type is
/// `kind`. Returns the record.
fn assert_failure(out: &Output, code: i32, kind: &str) -> Value {
    let text = stderr(out);
    let records: Vec<(usize, Value)> = text
        .lines()
        .enumerate()
        .filter_map(|(n, line)| {
            serde_json::from_str::<Value>(line).ok().map(|v| (n, v))
        })
        .filter(|(_, v)| v.get("error").is_some())
        .collect();
    let last = text.lines().count() - 1;

    assert_eq!(out.status.code(), Some(code), "stderr: {text}");
    assert_eq!(records.len(), 1, "error records in: {text}");
    assert_eq!(records[0].0, last, "record is not last: {text}");
    assert_eq!(records[0].1["error"]["type"], kind, "stderr: {text}");

    records[0].1.clone()
}

/// Returns a base URL on which nothing listens.
fn unreachable_url() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    drop(listener);

    format!("http://127.0.0.1:{port}")
}

/// Sends SIGINT to `pid`.
#[cfg(unix)]
fn interrupt(pid: u32) {
    let status = process::Command::new("kill")
        .args(["-INT", &pid.to_string()])
        .status()
        .unwrap();

    assert!(status.success(), "kill failed: {status}");
}

/// Waits up to `limit` for `child` to exit and returns its status.
#[cfg(unix)]
fn wait_within(
    child: &mut process::Child,
    limit: Duration,
) -> process::ExitStatus {
    let start = Instant::now();

    loop {
        if let Some(status) = child.try_wait().unwrap() {
            return status;
        }

        if start.elapsed() > limit {
            child.kill().unwrap();
            panic!("no exit within {limit:?}");
        }

        thread::sleep(Duration::from_millis(20));
    }
}

/// Every failing invocation used across these tests, as (args, stdin,
/// key, mock status, mock body, exit code, error type).
type FailureCase = (
    &'static [&'static str],
    &'static str,
    Option<&'static str>,
    u16,
    &'static str,
    i32,
    &'static str,
);

#[test]
fn should_print_help_snapshots() {
    let sandbox = Sandbox::new();
    let cases = [
        (vec!["--help"], "help.txt"),
        (vec!["raw", "--help"], "help-raw.txt"),
        (vec!["session", "--help"], "help-session.txt"),
        (vec!["task", "--help"], "help-task.txt"),
        (vec!["review", "--help"], "help-review.txt"),
    ];

    for (args, fixture) in cases {
        let out = sandbox.run("http://unused", None, &args, "");
        let want =
            fs::read_to_string(Path::new("tests/fixtures/cli").join(fixture))
                .unwrap();

        assert_eq!(out.status.code(), Some(0), "{args:?}");
        assert_eq!(stdout(&out), want, "{args:?}");
    }
}

#[test]
fn should_print_version() {
    let out = Sandbox::new().run("http://unused", None, &["--version"], "");

    assert_eq!(out.status.code(), Some(0));
    assert_eq!(stdout(&out), "overseer-judge 0.2.0\n");
}

#[test]
fn should_accept_single_dash_long_flags() {
    let sandbox = Sandbox::new();
    let args = ["raw", "-dry-run", "-input", "-"];

    let compact = sandbox.run(
        "http://unused",
        None,
        &[&args[..], &["-pretty=false"]].concat(),
        RAW_INPUT,
    );
    let pretty = sandbox.run(
        "http://unused",
        None,
        &[&args[..], &["-pretty=true"]].concat(),
        RAW_INPUT,
    );

    assert_eq!(compact.status.code(), Some(0), "{}", stderr(&compact));
    assert_eq!(stdout(&compact).lines().count(), 1);
    assert_eq!(stdout_json(&compact)["method"], "POST");
    assert!(stdout(&pretty).starts_with("{\n  \"method\": \"POST\",\n"));
    assert_eq!(stdout_json(&pretty), stdout_json(&compact));
}

#[test]
fn should_not_rewrite_flag_values_or_args_after_double_dash() {
    let sandbox = Sandbox::new();

    let session = sandbox.run(
        "http://unused",
        None,
        &[
            "session",
            "--input",
            "-",
            "--timeout",
            "-1s",
            "--model",
            "-preview",
            "--dry-run",
        ],
        "some pane text\n",
    );
    let task =
        sandbox.run("http://unused", None, &["task", "--", "-weird.md"], "");

    assert_eq!(session.status.code(), Some(0), "{}", stderr(&session));
    assert_eq!(stdout_json(&session)["body"]["model"], "-preview");
    let record = assert_failure(&task, 2, "usage");
    let message = record["error"]["message"].as_str().unwrap();
    assert!(message.contains("-weird.md"), "{message}");
    assert!(!message.contains("--weird.md"), "{message}");
}

#[test]
fn should_disable_timeout_for_zero() {
    let server = MockServer::start();
    serve(&server, 200, RAW_RESPONSE, Duration::from_millis(1500));
    let sandbox = Sandbox::new();
    let raw = ["raw", "--input", "-", "--timeout"];

    let unbounded = sandbox.run(
        &server.base_url(),
        Some(KEY),
        &[&raw[..], &["0"]].concat(),
        RAW_INPUT,
    );
    let bounded = sandbox.run(
        &server.base_url(),
        Some(KEY),
        &[&raw[..], &["200ms"]].concat(),
        RAW_INPUT,
    );

    assert_eq!(unbounded.status.code(), Some(0), "{}", stderr(&unbounded));
    assert_failure(&bounded, 8, "network");
}

#[test]
fn should_exit_2_with_usage_record_when_no_verb() {
    let out = Sandbox::new().run("http://unused", None, &[], "");

    assert_failure(&out, 2, "usage");
    assert!(out.stdout.is_empty());
}

#[test]
fn should_exit_2_with_usage_record_when_verb_unknown() {
    let out = Sandbox::new().run("http://unused", None, &["bogus"], "");

    assert_failure(&out, 2, "usage");
    assert!(stderr(&out).contains("bogus"));
}

#[test]
fn should_map_every_status_to_exit_code() {
    let sandbox = Sandbox::new();
    let cases = [
        (401, "{}", 3, "auth", Some(401_u16)),
        (422, r#"{"error":"bad"}"#, 5, "request", Some(422)),
        (429, "{}", 6, "rate_limit", Some(429)),
        (500, "{}", 7, "server", Some(500)),
        (200, "[]", 7, "response", None),
    ];

    for (status, body, code, kind, want_status) in cases {
        let server = MockServer::start();
        serve(&server, status, body, Duration::ZERO);

        let out = sandbox.run(
            &server.base_url(),
            Some(KEY),
            &["raw", "--input", "-"],
            RAW_INPUT,
        );

        let record = assert_failure(&out, code, kind);
        assert_eq!(
            record["error"]["status"].as_u64(),
            want_status.map(u64::from),
            "{status}"
        );
    }

    let out = sandbox.run(
        &unreachable_url(),
        Some(KEY),
        &["raw", "--input", "-"],
        RAW_INPUT,
    );
    assert_failure(&out, 8, "network");
}

#[test]
fn should_exit_3_without_request_when_no_key() {
    let server = MockServer::start();
    let mock = serve(&server, 200, SESSION_RESPONSE, Duration::ZERO);
    let sandbox = Sandbox::new();

    let out = sandbox.run(
        &server.base_url(),
        None,
        &["session", "--input", "-"],
        "pane\n",
    );

    assert_failure(&out, 3, "auth");
    assert_eq!(mock.calls(), 0);
}

#[test]
fn should_exit_3_for_static_only_task_without_key() {
    let server = MockServer::start();
    let mock = serve(&server, 200, "{}", Duration::ZERO);
    let sandbox = Sandbox::new();
    let path = sandbox.file("task.md", STATIC_ONLY_TASK);

    let out = sandbox.run(
        &server.base_url(),
        None,
        &["task", path.to_str().unwrap()],
        "",
    );

    assert_failure(&out, 3, "auth");
    assert!(out.stdout.is_empty());
    assert_eq!(mock.calls(), 0);
}

#[test]
fn should_print_raw_success_golden() {
    let server = MockServer::start();
    serve(&server, 200, RAW_RESPONSE, Duration::ZERO);

    let out = Sandbox::new().run(
        &server.base_url(),
        Some(KEY),
        &["raw", "--input", "-"],
        RAW_INPUT,
    );

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    assert_eq!(
        stdout(&out),
        r#"{"model":"jev-1","answers":{"a":{"type":"noul","noul":0.25},"b":{"type":"choice"}},"usage":{"input_tokens":12,"output_tokens":3}}"#
            .to_owned()
            + "\n"
    );
}

#[test]
fn should_insert_model_in_raw_dry_run() {
    let sandbox = Sandbox::new();
    let own = r#"{"state":"x","questions":{},"model":"own"}"#;
    let cases: [(&[&str], &str, &str); 4] = [
        (&[], RAW_INPUT, ENV_MODEL),
        (&["--model", "flag-model"], RAW_INPUT, "flag-model"),
        (&["--model", ""], RAW_INPUT, ENV_MODEL),
        (&["--model", "flag-model"], own, "own"),
    ];

    for (extra, input, want) in cases {
        let args = [&["raw", "--dry-run", "--input", "-"], extra].concat();

        let out = sandbox.run("http://unused", None, &args, input);

        assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
        assert_eq!(stdout_json(&out)["body"]["model"], want, "{extra:?}");
    }
}

#[test]
fn should_keep_nested_raw_input_verbatim() {
    let input = r#"{"state": {"z": 1, "a": {"y": 2, "b": [3, {"q": 0, "c": 1}]}},
        "questions": {}}"#;

    let out = Sandbox::new().run(
        "http://unused",
        None,
        &["raw", "--dry-run", "--input", "-"],
        input,
    );

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    assert!(
        stdout(&out)
            .contains(r#""state":{"z":1,"a":{"y":2,"b":[3,{"q":0,"c":1}]}}"#),
        "{}",
        stdout(&out)
    );
}

#[test]
fn should_reject_pretty_on_review() {
    let sandbox = Sandbox::new();
    let path = sandbox.file("round.md", REVIEW_ROUND);

    let out = sandbox.run(
        "http://unused",
        Some(KEY),
        &["review", "--pretty", path.to_str().unwrap()],
        "",
    );

    let record = assert_failure(&out, 2, "usage");
    assert_eq!(
        record["error"]["message"],
        "review does not accept --pretty: its output is JSON Lines, one \
         item per line"
    );
}

#[test]
fn should_print_nothing_for_review_with_no_items_and_need_no_key() {
    let sandbox = Sandbox::new();

    let out = sandbox.run(
        "http://unused",
        None,
        &["review", "-"],
        "# Review round 1\n\nNothing found.\n",
    );

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    assert!(out.stdout.is_empty());
}

/// Mocks one answer per review item; the second is delayed by `delay`
/// and answers with `second_status`.
fn review_mocks(
    server: &MockServer,
    delay: Duration,
    second_status: u16,
) -> (httpmock::Mock<'_>, httpmock::Mock<'_>) {
    let answer = r#"{"model":"jev-1","answers":{
        "style_only":{"type":"noul","noul":0.12},
        "response_0":{"type":"choice","choice":"fixed","confidence":0.77,
        "probabilities":{"fixed":0.77,"concern":0.23}}},
        "usage":{"input_tokens":900,"output_tokens":6}}"#;
    let first = server.mock(|when, then| {
        when.method(POST).body_includes("\"R1-01\"");
        then.status(200).body(answer);
    });
    let second = server.mock(|when, then| {
        when.method(POST).body_includes("\"R1-02\"");
        then.status(second_status).body(answer).delay(delay);
    });

    (first, second)
}

#[test]
fn should_stream_review_lines() {
    let server = MockServer::start();
    let (first, second) = review_mocks(&server, Duration::from_secs(3), 200);
    let sandbox = Sandbox::new();
    let path = sandbox.file("round.md", REVIEW_ROUND);
    let start = Instant::now();

    let mut child = sandbox
        .command(
            &server.base_url(),
            Some(KEY),
            &["review", path.to_str().unwrap()],
        )
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let mut lines = BufReader::new(child.stdout.take().unwrap());
    let mut line = String::new();
    lines.read_line(&mut line).unwrap();
    let first_after = start.elapsed();
    let status = child.wait().unwrap();
    let mut rest = String::new();
    lines.read_line(&mut rest).unwrap();

    assert!(first_after < Duration::from_secs(1), "{first_after:?}");
    assert_eq!(serde_json::from_str::<Value>(&line).unwrap()["id"], "R1-01");
    assert_eq!(status.code(), Some(0));
    assert_eq!(serde_json::from_str::<Value>(&rest).unwrap()["id"], "R1-02");
    assert_eq!(first.calls() + second.calls(), 2);
}

#[test]
fn should_keep_first_line_when_second_item_fails() {
    let server = MockServer::start();
    review_mocks(&server, Duration::ZERO, 500);
    let sandbox = Sandbox::new();
    let path = sandbox.file("round.md", REVIEW_ROUND);

    let out = sandbox.run(
        &server.base_url(),
        Some(KEY),
        &["review", path.to_str().unwrap()],
        "",
    );

    assert_failure(&out, 7, "server");
    let text = stdout(&out);
    assert_eq!(text.lines().count(), 1, "{text}");
    assert_eq!(serde_json::from_str::<Value>(&text).unwrap()["id"], "R1-01");
}

#[test]
fn should_print_task_static_only_dry_run_when_not_judgeable() {
    let sandbox = Sandbox::new();
    let path = sandbox.file("task.md", STATIC_ONLY_TASK);

    let out = sandbox.run(
        "http://unused",
        None,
        &["task", "--dry-run", path.to_str().unwrap()],
        "",
    );

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(
        text.starts_with(r#"{"method":"","path":"","body":null,"static":["#),
        "{text}"
    );
    assert!(!stdout_json(&out)["static"].as_array().unwrap().is_empty());
}

#[test]
fn should_end_every_failure_with_one_error_record() {
    let cases: [FailureCase; 13] = [
        (&[], "", None, 200, "{}", 2, "usage"),
        (&["bogus"], "", None, 200, "{}", 2, "usage"),
        (&["raw"], "", None, 200, "{}", 2, "usage"),
        (&["raw", "--input", "-"], "[1]", None, 200, "{}", 2, "usage"),
        (
            &["review", "--pretty", "-"],
            "",
            None,
            200,
            "{}",
            2,
            "usage",
        ),
        (
            &["task", "--", "-weird.md"],
            "",
            None,
            200,
            "{}",
            2,
            "usage",
        ),
        (
            &["raw", "--input", "-"],
            RAW_INPUT,
            None,
            200,
            "{}",
            3,
            "auth",
        ),
        (&["task", "-"], STATIC_ONLY_TASK, None, 200, "{}", 3, "auth"),
        (
            &["raw", "--input", "-"],
            RAW_INPUT,
            Some(KEY),
            401,
            "",
            3,
            "auth",
        ),
        (
            &["raw", "--input", "-"],
            RAW_INPUT,
            Some(KEY),
            422,
            "no",
            5,
            "request",
        ),
        (
            &["raw", "--input", "-"],
            RAW_INPUT,
            Some(KEY),
            500,
            "",
            7,
            "server",
        ),
        (
            &["raw", "--input", "-"],
            RAW_INPUT,
            Some(KEY),
            200,
            "[]",
            7,
            "response",
        ),
        (
            &["session", "--input", "/nonexistent/file"],
            "",
            Some(KEY),
            200,
            "{}",
            2,
            "usage",
        ),
    ];
    let sandbox = Sandbox::new();

    for (args, stdin, key, status, body, code, kind) in cases {
        let server = MockServer::start();
        serve(&server, status, body, Duration::ZERO);

        let out = sandbox.run(&server.base_url(), key, args, stdin);

        assert_failure(&out, code, kind);
    }
}

#[test]
fn should_emit_utf8_without_bom_or_cr() {
    let server = MockServer::start();
    serve(&server, 200, SESSION_RESPONSE, Duration::ZERO);
    let sandbox = Sandbox::new();

    let outputs = [
        sandbox.run(
            &server.base_url(),
            Some(KEY),
            &["session", "--input", "-", "--pretty"],
            "héllo ✓\r\n> ",
        ),
        sandbox.run(
            "http://unused",
            None,
            &["raw", "--dry-run", "--input", "-"],
            r#"{"state":"naïve ✓","questions":{}}"#,
        ),
        sandbox.run("http://unused", None, &["bogus"], ""),
    ];

    for out in outputs {
        for bytes in [&out.stdout, &out.stderr] {
            assert!(std::str::from_utf8(bytes).is_ok());
            assert!(!bytes.starts_with(b"\xEF\xBB\xBF"));
            assert!(!bytes.contains(&b'\r'), "{}", stdout(&out));
        }
    }
}

#[test]
fn should_redact_key_in_error_record() {
    let server = MockServer::start();
    serve(
        &server,
        422,
        &format!(r#"{{"error":"bad key {KEY}"}}"#),
        Duration::ZERO,
    );

    let out = Sandbox::new().run(
        &server.base_url(),
        Some(KEY),
        &["raw", "--input", "-"],
        RAW_INPUT,
    );

    let record = assert_failure(&out, 5, "request");
    assert!(!stderr(&out).contains(KEY), "{}", stderr(&out));
    assert!(
        record["error"]["message"]
            .as_str()
            .unwrap()
            .contains("[redacted]")
    );
}

#[cfg(unix)]
#[test]
fn should_exit_130_on_sigint() {
    let server = MockServer::start();
    serve(&server, 200, RAW_RESPONSE, Duration::from_secs(5));
    let sandbox = Sandbox::new();
    let path = sandbox.file("raw.json", RAW_INPUT);

    let child = sandbox
        .command(
            &server.base_url(),
            Some(KEY),
            &["raw", "--input", path.to_str().unwrap()],
        )
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    thread::sleep(Duration::from_millis(300));
    interrupt(child.id());
    let out = child.wait_with_output().unwrap();

    assert_failure(&out, 130, "interrupted");
}

#[cfg(unix)]
#[test]
fn should_exit_130_on_sigint_while_reading_stdin() {
    let sandbox = Sandbox::new();

    let mut child = sandbox
        .command("http://unused", Some(KEY), &["session", "--input", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    // Held open, never closed, so the read blocks until the signal.
    let _stdin = child.stdin.take().unwrap();
    thread::sleep(Duration::from_millis(300));
    interrupt(child.id());
    let status = wait_within(&mut child, Duration::from_secs(2));
    let out = Output {
        status,
        stdout: Vec::new(),
        stderr: {
            let mut bytes = Vec::new();
            std::io::Read::read_to_end(
                &mut child.stderr.take().unwrap(),
                &mut bytes,
            )
            .unwrap();
            bytes
        },
    };

    assert_failure(&out, 130, "interrupted");
}
