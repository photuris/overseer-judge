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
- status: open
The first body.
- response: fixed: done

### R1-02: Second finding
- severity: low
- status: open
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

/// Asserts `out` exited 130 and that stderr is exactly one line: the
/// `interrupted` error record.
#[cfg(unix)]
fn assert_interrupted(out: &Output) {
    assert_failure(out, 130, "interrupted");
    assert_eq!(stderr(out).lines().count(), 1, "stderr: {}", stderr(out));
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
    assert_eq!(
        stdout(&out),
        concat!("overseer-judge ", env!("CARGO_PKG_VERSION"), "\n")
    );
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

    let out = sandbox.run("http://unused", None, &["review", "-"], "\n  \n");

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    assert!(out.stdout.is_empty());
}

/// The review fixture `name`'s path, relative to the crate root.
fn review_fixture(name: &str) -> String {
    format!("tests/fixtures/review/{name}")
}

/// Runs `review --dry-run` on the review fixture `name` without a key.
fn review_dry_run(name: &str, extra: &[&str]) -> Output {
    let fixture = review_fixture(name);
    let mut args = vec!["review", "--dry-run", fixture.as_str()];
    args.extend(extra);

    Sandbox::new().run("http://unused", None, &args, "")
}

/// Parses each stdout line of `out` as JSON.
fn stdout_lines(out: &Output) -> Vec<Value> {
    stdout(out)
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect()
}

/// The `warnings` arrays `mismatch-01.md` yields, in item order.
const MISMATCH_WARNINGS: [&[&str]; 2] = [
    &["missing_severity", "missing_status", "unparsed_response"],
    &["text_after_response"],
];

/// Asserts the raw JSON `line` ends with a `warnings` key equal to
/// `want`. Checked on the text: a parsed `Value` sorts its keys.
fn assert_warnings_last(line: &str, want: &[&str]) {
    let tail =
        format!(",\"warnings\":{}}}", serde_json::to_string(want).unwrap());
    assert!(line.ends_with(&tail), "want suffix {tail}: {line}");
}

#[test]
fn should_exit_2_when_review_file_has_content_but_no_items() {
    for extra in [&[][..], &["--dry-run"][..]] {
        let fixture = review_fixture("no-items-01.md");
        let mut args = vec!["review", fixture.as_str()];
        args.extend(extra);

        let out = Sandbox::new().run("http://unused", None, &args, "");

        assert!(out.stdout.is_empty(), "{extra:?}: {}", stdout(&out));
        let record = assert_failure(&out, 2, "usage");
        let message = record["error"]["message"].as_str().unwrap();
        assert!(message.starts_with("no review items in"), "{message}");
    }
}

#[test]
fn should_print_review_warnings_in_dry_run() {
    let out = review_dry_run("mismatch-01.md", &[]);

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    let text = stdout(&out);
    assert_eq!(text.lines().count(), 2, "{text}");
    for (line, want) in text.lines().zip(MISMATCH_WARNINGS) {
        assert_warnings_last(line, want);
    }
}

#[test]
fn should_not_add_warnings_to_well_formed_rounds() {
    for name in ["round-01.md", "round-02.md", "round-03.md"] {
        let out = review_dry_run(name, &[]);

        assert_eq!(out.status.code(), Some(0), "{name}: {}", stderr(&out));
        assert_eq!(stderr(&out), "", "{name}");
        let records = stdout_lines(&out);
        assert!(!records.is_empty(), "{name}");
        for record in &records {
            assert!(record.get("warnings").is_none(), "{name}: {record}");
        }
    }
}

#[test]
fn should_carry_warnings_on_typed_records() {
    let server = MockServer::start();
    let (first, second) = review_mocks(&server, Duration::ZERO, 200);
    let sandbox = Sandbox::new();
    let fixture = review_fixture("mismatch-01.md");

    let out =
        sandbox.run(&server.base_url(), Some(KEY), &["review", &fixture], "");

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    first.assert_calls(1);
    second.assert_calls(1);
    let text = stdout(&out);
    assert_eq!(text.lines().count(), 2, "{text}");
    for (line, want) in text.lines().zip(MISMATCH_WARNINGS) {
        let record: Value = serde_json::from_str(line).unwrap();
        assert!(record.get("style_only").is_some(), "{record}");
        assert_warnings_last(line, want);
    }
}

#[test]
fn should_log_each_review_warning_once_at_warn() {
    let out = review_dry_run("mismatch-01.md", &[]);
    let text = stderr(&out);
    let want = MISMATCH_WARNINGS
        .iter()
        .zip(["R1-01", "R1-02"])
        .flat_map(|(names, id)| names.iter().map(move |name| (id, *name)));

    assert_eq!(out.status.code(), Some(0), "{text}");
    assert_eq!(text.lines().count(), 4, "{text}");
    for (id, name) in want {
        let hits = text
            .lines()
            .filter(|line| line.contains("WARN") && line.contains(name))
            .collect::<Vec<_>>();
        assert_eq!(hits.len(), 1, "{name}: {text}");
        assert!(hits[0].contains(&format!("id={id}")), "{name}: {text}");
    }

    let quiet = review_dry_run("mismatch-01.md", &["--log-level", "error"]);
    assert_eq!(quiet.status.code(), Some(0));
    assert_eq!(stderr(&quiet), "");
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

    assert_interrupted(&out);
}

#[cfg(unix)]
#[test]
fn should_exit_130_on_sigint_during_connect() {
    let sandbox = Sandbox::new();
    let path = sandbox.file("raw.json", RAW_INPUT);

    // A non-routable address, so the connect blocks until the signal
    // interrupts it (EINTR).
    let child = sandbox
        .command(
            "http://10.255.255.1",
            Some(KEY),
            &["raw", "--input", path.to_str().unwrap(), "--timeout", "30s"],
        )
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    thread::sleep(Duration::from_millis(300));
    interrupt(child.id());
    let out = child.wait_with_output().unwrap();

    assert_interrupted(&out);
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

    assert_interrupted(&out);
}

#[cfg(unix)]
#[test]
fn should_write_one_interrupt_record_when_stderr_is_full() {
    use std::{
        io::{ErrorKind, Read, Write},
        os::unix::net::UnixStream,
    };

    // A socket stands in for the pipe: std can make it non-blocking,
    // which is how the buffer is filled to the last byte.
    let (mut reader, mut writer) = UnixStream::pair().unwrap();
    writer.set_nonblocking(true).unwrap();
    let mut filled = 0;
    loop {
        match writer.write(&[b'x'; 4096]) {
            Ok(n) => filled += n,
            Err(err) if err.kind() == ErrorKind::WouldBlock => break,
            Err(err) => panic!("fill: {err}"),
        }
    }
    writer.set_nonblocking(false).unwrap();
    let sandbox = Sandbox::new();
    let mut cmd = sandbox.command(
        "http://unused",
        None,
        &["raw", "--dry-run", "--input", "-"],
    );
    cmd.stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::from(std::os::fd::OwnedFd::from(writer)));
    let mut child = cmd.spawn().unwrap();
    // Drop the command's copy of the write end, so EOF follows the
    // child's exit.
    drop(cmd);

    thread::sleep(Duration::from_millis(300));
    interrupt(child.id());
    thread::sleep(Duration::from_millis(200));
    drop(child.stdin.take());
    thread::sleep(Duration::from_millis(300));
    assert!(
        child.try_wait().unwrap().is_none(),
        "exited before stderr had room for the record"
    );
    let mut drained = vec![0; filled];
    reader.read_exact(&mut drained).unwrap();
    let status = wait_within(&mut child, Duration::from_secs(5));
    let mut rest = Vec::new();
    reader.read_to_end(&mut rest).unwrap();
    let out = Output {
        status,
        stdout: Vec::new(),
        stderr: rest,
    };

    assert_interrupted(&out);
}

#[cfg(unix)]
#[test]
fn should_never_cut_a_stdout_record_on_sigint() {
    let sandbox = Sandbox::new();
    let round = format!(
        "### R1-01: Finding\n- severity: minor\n- status: open\n\n{}\n",
        "x".repeat(1 << 20)
    );
    let path = sandbox.file("round.md", &round);

    let child = sandbox
        .command("http://unused", None, &["review", "--dry-run", "-"])
        .stdin(fs::File::open(path).unwrap())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    // stdout stays unread until after the signal, so the record is
    // stuck mid-write when it arrives.
    thread::sleep(Duration::from_millis(300));
    interrupt(child.id());
    let out = child.wait_with_output().unwrap();

    assert!(
        out.stdout.is_empty() || out.stdout.ends_with(b"\n"),
        "stdout cut after {} bytes",
        out.stdout.len()
    );
    for line in stdout(&out).lines() {
        serde_json::from_str::<Value>(line).unwrap();
    }
    if out.status.code() == Some(0) {
        assert_eq!(stderr(&out), "");
    } else {
        assert_interrupted(&out);
    }
}

#[test]
fn should_let_the_last_repeated_flag_win() {
    let server = MockServer::start();
    serve(&server, 200, RAW_RESPONSE, Duration::from_millis(300));
    let sandbox = Sandbox::new();
    let raw = ["raw", "--input", "-"];

    let string = sandbox.run(
        "http://unused",
        None,
        &[
            &raw[..],
            &["--dry-run", "--model", "first", "-model", "last"],
        ]
        .concat(),
        RAW_INPUT,
    );
    let duration = sandbox.run(
        &server.base_url(),
        Some(KEY),
        &[&raw[..], &["-timeout", "1ms", "--timeout", "0"]].concat(),
        RAW_INPUT,
    );
    let boolean = sandbox.run(
        "http://unused",
        None,
        &[
            &raw[..],
            &["--dry-run=false", "-dry-run", "--pretty", "-pretty=false"],
        ]
        .concat(),
        RAW_INPUT,
    );
    let session = |agents: &[&str]| {
        let args =
            [&["session", "--dry-run", "--input", "-"], agents].concat();
        let out = sandbox.run("http://unused", None, &args, "tail");
        assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));

        stdout_json(&out)
    };

    assert_eq!(string.status.code(), Some(0), "{}", stderr(&string));
    assert_eq!(stdout_json(&string)["body"]["model"], "last");
    assert_eq!(duration.status.code(), Some(0), "{}", stderr(&duration));
    assert_eq!(boolean.status.code(), Some(0), "{}", stderr(&boolean));
    assert_eq!(stdout(&boolean).lines().count(), 1);
    assert_eq!(
        session(&["--agent", "claude", "-agent", "pi"]),
        session(&["--agent", "pi"])
    );
    assert_ne!(session(&["--agent", "pi"]), session(&["--agent", "claude"]));
}

#[test]
fn should_read_dash_prefixed_input_files() {
    let sandbox = Sandbox::new();
    sandbox.file("-weird.json", RAW_INPUT);
    sandbox.file("-weird.txt", "tail");
    let run = |args: &[&str]| {
        Command::from_std(sandbox.command("http://unused", None, args))
            .current_dir(sandbox.dir.path())
            .output()
            .unwrap()
    };

    let raw = run(&["raw", "--dry-run", "--input", "-weird.json"]);
    let session = run(&["session", "--dry-run", "-input", "-weird.txt"]);

    assert_eq!(raw.status.code(), Some(0), "{}", stderr(&raw));
    assert_eq!(stdout_json(&raw)["body"]["model"], ENV_MODEL);
    assert_eq!(session.status.code(), Some(0), "{}", stderr(&session));
    assert!(stdout(&session).contains("tail"), "{}", stdout(&session));
}

#[test]
fn should_accept_go_boolean_spellings() {
    let sandbox = Sandbox::new();
    let spellings = [
        ("1", true),
        ("t", true),
        ("T", true),
        ("TRUE", true),
        ("true", true),
        ("True", true),
        ("0", false),
        ("f", false),
        ("F", false),
        ("FALSE", false),
        ("false", false),
        ("False", false),
    ];

    for (n, (value, want)) in spellings.into_iter().enumerate() {
        let dash = if n % 2 == 0 { "--" } else { "-" };
        let dry_run = format!("{dash}dry-run={value}");
        let pretty = format!("{dash}pretty={value}");

        // Without a key, a real request fails with exit 3.
        let sent = sandbox.run(
            "http://unused",
            None,
            &["raw", "--input", "-", &dry_run],
            RAW_INPUT,
        );
        let shown = sandbox.run(
            "http://unused",
            None,
            &["raw", "--dry-run", "--input", "-", &pretty],
            RAW_INPUT,
        );

        assert_eq!(
            sent.status.code(),
            Some(if want { 0 } else { 3 }),
            "{dry_run}: {}",
            stderr(&sent)
        );
        assert_eq!(shown.status.code(), Some(0), "{}", stderr(&shown));
        assert_eq!(stdout(&shown).lines().count() > 1, want, "{pretty}");
    }

    let yes = sandbox.run(
        "http://unused",
        None,
        &["raw", "--dry-run", "--input", "-", "--pretty=yes"],
        RAW_INPUT,
    );
    assert_failure(&yes, 2, "usage");
}

#[test]
fn should_disable_timeout_for_subnanosecond_values() {
    let server = MockServer::start();
    serve(&server, 200, RAW_RESPONSE, Duration::from_millis(300));
    let sandbox = Sandbox::new();

    let out = sandbox.run(
        &server.base_url(),
        Some(KEY),
        &["raw", "--input", "-", "--timeout", ".5ns"],
        RAW_INPUT,
    );

    assert_eq!(out.status.code(), Some(0), "{}", stderr(&out));
    assert_eq!(stdout_json(&out)["model"], "jev-1");
}
