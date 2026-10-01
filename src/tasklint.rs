//! The `tasklint` verb: static and Jev-backed checks of an overseer task file.
//!
//! [`parse`] reads a task file with a fence-aware line scanner,
//! [`static_checks`] runs the six structural checks that need no
//! model, and [`judge`] asks Jev the three questions that need
//! judgment.

use std::collections::BTreeMap;

use serde::Serialize;
use serde_json::{Value, json};

use crate::jev;

/// The acceptance score at or above which a task's Expect lines are
/// read as sound. Provisional: it was fitted to five fixtures, so
/// re-measure before trusting it on new ones.
pub const SOUND_CUT: f64 = 2.2;

/// The headings the template demands, in template order.
const REQUIRED: [&str; 8] = [
    "Objective",
    "Files",
    "Out of scope",
    "Acceptance",
    "Budget",
    "Rules",
    "Result",
    "Verification",
];

/// The values a status line may carry.
const STATUSES: [&str; 4] = ["ready", "in-progress", "done", "accepted"];

/// The `noul` questions whose answers land in [`Report::judgments`].
const NOULS: [&str; 2] = ["needs_interpretation", "scope_generic"];

/// Instruction text for the `needs_interpretation` question.
const INTERPRETATION_INSTRUCTIONS: &str = "The state is one task \
specification written for a less capable coding agent that executes specs \
literally. `objective` says what must exist; `spec` (possibly empty) gives \
interfaces and behavior. Does completing the task require the agent to \
make design decisions, choose between approaches, or guess intent, rather \
than execute steps and interfaces the spec states?";

/// Instruction text for the `acceptance_strength` question.
const STRENGTH_INSTRUCTIONS: &str = "`acceptance` lists shell commands, \
each with an `Expect:` line, used to decide whether a coding task \
described by `objective` was completed correctly. Rate how well the \
Expect lines would catch a wrong or incomplete implementation.";

/// Instruction text for the `scope_generic` question.
const SCOPE_INSTRUCTIONS: &str = "`out_of_scope` should name the specific \
tempting adjacent work an agent might do while completing `objective`. Is \
it generic instead, saying only 'anything else' or restating that \
unlisted files are off limits, without naming concrete adjacent work?";

// ── Parse ───────────────────────────────────────────────────────────────────

/// A parsed task file.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct Doc {
    /// The H1 text without its `# `.
    pub title: String,
    /// The value of the status line under the H1, or `""`.
    pub status: String,
    /// Section names in document order, first occurrence only.
    pub order: Vec<String>,
    /// Section name to body: the lines between its heading and the
    /// next one, each keeping its newline. The first occurrence of a
    /// name wins.
    pub sections: BTreeMap<String, String>,
}

impl Doc {
    /// Returns the body of section `name`, or `""` when absent.
    #[must_use]
    pub fn section(&self, name: &str) -> &str {
        self.sections.get(name).map_or("", String::as_str)
    }

    /// Reports whether the document holds the two sections the model
    /// questions read: without an Objective or an Acceptance there is
    /// nothing to judge.
    #[must_use]
    pub fn judgeable(&self) -> bool {
        ["Objective", "Acceptance"]
            .iter()
            .all(|name| self.sections.contains_key(*name))
    }
}

/// Parses `text` line by line.
///
/// A line starting with ```` ``` ```` toggles fenced mode, and inside a
/// fence no line is a heading or an H1. A repeated `## ` heading does
/// not open a section: its line and the lines after it stay with the
/// section it interrupts. Only the first non-empty line after the H1
/// can carry the status, whatever else that line turns out to be.
#[must_use]
pub fn parse(text: &str) -> Doc {
    let mut doc = Doc::default();
    let mut fenced = false;
    let mut after_title = false;
    let mut section: Option<String> = None;
    let mut body = String::new();

    for line in text.strip_suffix('\n').unwrap_or(text).split('\n') {
        if after_title && !line.trim().is_empty() {
            after_title = false;
            if let Some(status) = status_of(line) {
                status.clone_into(&mut doc.status);
            }
        }

        if line.starts_with("```") {
            fenced = !fenced;
        } else if fenced {
        } else if let Some(name) = heading(line)
            && !doc.order.iter().any(|seen| seen == name)
        {
            flush(&mut doc, section.take(), &mut body);
            doc.order.push(name.to_owned());
            section = Some(name.to_owned());
            continue;
        } else if doc.title.is_empty()
            && let Some(title) = line.strip_prefix("# ")
        {
            title.trim().clone_into(&mut doc.title);
            after_title = true;
            continue;
        }

        if section.is_some() {
            body.push_str(line);
            body.push('\n');
        }
    }
    flush(&mut doc, section, &mut body);

    doc
}

/// Stores `body` under `section`, if any, and empties `body`.
fn flush(doc: &mut Doc, section: Option<String>, body: &mut String) {
    if let Some(name) = section {
        doc.sections.insert(name, std::mem::take(body));
    }
    body.clear();
}

/// Returns the trimmed name of a `## ` heading line, or `None` when
/// the line is not one or names nothing.
fn heading(line: &str) -> Option<&str> {
    line.strip_prefix("## ")
        .map(str::trim)
        .filter(|name| !name.is_empty())
}

/// Returns the status a `Status: <value>` line carries: the run of
/// non-space characters after the prefix (Go's ASCII `\S+`).
fn status_of(line: &str) -> Option<&str> {
    let rest = line.strip_prefix("Status: ")?;
    let end = rest
        .find(['\t', '\n', '\x0C', '\r', ' '])
        .unwrap_or(rest.len());

    (end > 0).then(|| &rest[..end])
}

// ── Static checks ───────────────────────────────────────────────────────────

/// One static check result.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Finding {
    /// The check's name.
    pub check: String,
    /// Whether the check passed.
    pub ok: bool,
    /// Why the check failed; empty when it passed.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub detail: String,
}

impl Finding {
    /// Returns a passing finding for `check`.
    fn pass(check: &str) -> Self {
        Self {
            check: check.to_owned(),
            ok: true,
            detail: String::new(),
        }
    }

    /// Returns a failing finding for `check` with `detail`.
    fn fail(check: &str, detail: impl Into<String>) -> Self {
        Self {
            check: check.to_owned(),
            ok: false,
            detail: detail.into(),
        }
    }
}

/// Runs the structural checks that need no model: always all six, in
/// the order `status_line`, `sections_present`, `sections_ordered`,
/// `allowed_nonempty`, `acceptance_command`, `budget_numeric`.
#[must_use]
pub fn static_checks(doc: &Doc) -> Vec<Finding> {
    vec![
        status_line(doc),
        sections_present(doc),
        sections_ordered(doc),
        allowed_nonempty(doc),
        acceptance_command(doc),
        budget_numeric(doc),
    ]
}

/// Checks that the document declares a known status.
fn status_line(doc: &Doc) -> Finding {
    const CHECK: &str = "status_line";

    if STATUSES.contains(&doc.status.as_str()) {
        return Finding::pass(CHECK);
    }

    Finding::fail(
        CHECK,
        format!(
            "status {:?} is not one of {}",
            doc.status,
            STATUSES.join("|")
        ),
    )
}

/// Checks that every required heading exists. The detail is the
/// missing names alone, comma-separated, in template order.
fn sections_present(doc: &Doc) -> Finding {
    const CHECK: &str = "sections_present";

    let missing: Vec<&str> = REQUIRED
        .into_iter()
        .filter(|name| !doc.sections.contains_key(*name))
        .collect();

    if missing.is_empty() {
        Finding::pass(CHECK)
    } else {
        Finding::fail(CHECK, missing.join(", "))
    }
}

/// Checks that the required headings present keep their template
/// order. Missing headings are `sections_present`'s business.
fn sections_ordered(doc: &Doc) -> Finding {
    const CHECK: &str = "sections_ordered";

    let mut previous: Option<(usize, &str)> = None;
    for name in &doc.order {
        let Some(rank) = REQUIRED.iter().position(|req| req == name) else {
            continue;
        };
        if let Some((prev_rank, prev)) = previous
            && rank < prev_rank
        {
            return Finding::fail(
                CHECK,
                format!("{name} appears after {prev}"),
            );
        }
        previous = Some((rank, name));
    }

    Finding::pass(CHECK)
}

/// Checks that the Files body names at least one allowed path. The
/// list is comma-separated and may wrap until a blank line or a
/// `Read-only:` line.
fn allowed_nonempty(doc: &Doc) -> Finding {
    const CHECK: &str = "allowed_nonempty";

    let lines = unfenced(doc.section("Files"));
    let Some(start) = lines.iter().position(|l| l.starts_with("Allowed:"))
    else {
        return Finding::fail(CHECK, "no Allowed: line");
    };

    let first = &lines[start]["Allowed:".len()..];
    let rest = lines[start + 1..].iter().take_while(|line| {
        !line.trim().is_empty() && !line.starts_with("Read-only:")
    });
    let named = std::iter::once(&first)
        .chain(rest)
        .flat_map(|line| line.split(','))
        .any(|token| !token.trim().is_empty());

    if named {
        Finding::pass(CHECK)
    } else {
        Finding::fail(CHECK, "Allowed: names no paths")
    }
}

/// Checks that the Acceptance body holds at least one `Command:` line
/// and that each is answered by an `Expect:` line before the next
/// command.
fn acceptance_command(doc: &Doc) -> Finding {
    const CHECK: &str = "acceptance_command";

    let lines = unfenced(doc.section("Acceptance"));
    let mut commands = 0;
    for (i, line) in lines.iter().enumerate() {
        if !line.starts_with("Command:") {
            continue;
        }
        commands += 1;

        let expected = lines[i + 1..]
            .iter()
            .find(|l| l.starts_with("Expect:") || l.starts_with("Command:"))
            .is_some_and(|l| l.starts_with("Expect:"));
        if !expected {
            return Finding::fail(
                CHECK,
                format!("Command {commands} has no Expect: line"),
            );
        }
    }

    if commands == 0 {
        Finding::fail(CHECK, "no Command: line")
    } else {
        Finding::pass(CHECK)
    }
}

/// Checks that the Budget body states a turn count.
fn budget_numeric(doc: &Doc) -> Finding {
    const CHECK: &str = "budget_numeric";

    if has_turn_count(doc.section("Budget")) {
        Finding::pass(CHECK)
    } else {
        Finding::fail(CHECK, r#"no "<n> turns" in the budget"#)
    }
}

/// Reports whether `text` matches Go's `\b\d+ turns\b`, with ASCII
/// digits and ASCII word boundaries.
fn has_turn_count(text: &str) -> bool {
    /// Whether `byte` is an ASCII word character.
    fn word(byte: u8) -> bool {
        byte.is_ascii_alphanumeric() || byte == b'_'
    }

    let bytes = text.as_bytes();
    text.match_indices(" turns").any(|(at, needle)| {
        let digits = bytes[..at]
            .iter()
            .rev()
            .take_while(|b| b.is_ascii_digit())
            .count();
        let start = at - digits;

        digits > 0
            && (start == 0 || !word(bytes[start - 1]))
            && bytes.get(at + needle.len()).is_none_or(|b| !word(*b))
    })
}

/// Splits `body` into lines, dropping fences and the lines inside
/// them, so an example in a code block cannot satisfy a check.
fn unfenced(body: &str) -> Vec<&str> {
    let mut fenced = false;

    body.split('\n')
        .filter(|line| {
            if line.starts_with("```") {
                fenced = !fenced;
                return false;
            }
            !fenced
        })
        .collect()
}

// ── Questions and request ───────────────────────────────────────────────────

/// The graded acceptance judgment: `score` runs 0 (vacuous) to 3
/// (every Expect is specific).
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Strength {
    /// The acceptance score.
    pub score: f64,
    /// The model's confidence in the score.
    pub confidence: f64,
}

/// The full lint output for one task file.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Report {
    /// The file name as given by the caller.
    pub file: String,
    /// The six static findings.
    pub r#static: Vec<Finding>,
    /// The `noul` probabilities; `None` when the model was skipped.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub judgments: Option<BTreeMap<String, f64>>,
    /// The acceptance score; `None` when the model was skipped.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub acceptance: Option<Strength>,
    /// The model that answered; empty when skipped.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
    /// Token usage; zero when skipped.
    pub usage: jev::Usage,
}

/// Returns the three questions sent for every task judgment.
#[must_use]
pub fn questions() -> BTreeMap<String, jev::Question> {
    /// Builds a question of `kind` with `instructions` and `criteria`.
    fn question(
        kind: &str,
        instructions: &str,
        criteria: Value,
    ) -> jev::Question {
        jev::Question {
            r#type: kind.to_owned(),
            instructions: instructions.to_owned(),
            criteria: Some(criteria),
        }
    }

    BTreeMap::from([
        (
            "needs_interpretation".to_owned(),
            question(
                "noul",
                INTERPRETATION_INSTRUCTIONS,
                json!({
                    "true": "The objective or spec uses words like \
                        improve, clean up, make robust, handle properly, \
                        or describes an outcome without saying what to \
                        build; a capable engineer would need to decide \
                        the design before starting.",
                    "false": "The task names the concrete things that \
                        will exist (files, functions, types, behaviors) \
                        precisely enough that two engineers would build \
                        the same thing.",
                }),
            ),
        ),
        (
            // Worst level first: the index the model picks is the score.
            "acceptance_strength".to_owned(),
            question(
                "score",
                STRENGTH_INSTRUCTIONS,
                json!([
                    "The Expect lines check only that something runs, \
                    prints anything, or exits 0. A wrong or empty \
                    implementation would pass.",
                    "The Expect lines mostly check that commands or a \
                    test suite pass, with little or nothing tied to the \
                    specific behavior the objective describes.",
                    "Some Expect lines name specific output, strings, \
                    counts, or exit codes tied to the objective, but at \
                    least one only checks that a command or test suite \
                    passes.",
                    "Every Expect line names specific output, strings, \
                    counts, or exit codes tied to the behavior the \
                    objective describes. A wrong implementation would \
                    fail.",
                ]),
            ),
        ),
        (
            "scope_generic".to_owned(),
            question(
                "noul",
                SCOPE_INSTRUCTIONS,
                json!({
                    "true": "Out of scope is boilerplate; it names no \
                        specific feature, file, refactor, or dependency \
                        to avoid.",
                    "false": "Out of scope names at least one concrete \
                        thing an agent might plausibly do and forbids it.",
                }),
            ),
        ),
    ])
}

/// Builds the request state from `doc`; an absent section contributes
/// `""`.
#[must_use]
pub fn state(doc: &Doc) -> Value {
    json!({
        "objective": doc.section("Objective"),
        "files": doc.section("Files"),
        "out_of_scope": doc.section("Out of scope"),
        "acceptance": doc.section("Acceptance"),
        "spec": doc.section("Spec"),
    })
}

/// Returns the Jev request [`judge`] sends for `doc`, model left
/// `None` for the client or the caller to fill.
#[must_use]
pub fn request(doc: &Doc) -> jev::Request {
    jev::Request {
        state: state(doc),
        model: None,
        questions: questions(),
    }
}

/// Parses `text`, runs the static checks, and, when the document is
/// judgeable, asks the two nouls and the acceptance score. Otherwise
/// the report carries the static findings alone and no request is
/// made.
///
/// # Errors
///
/// Returns the client's [`jev::Error`], or [`jev::Error::Response`]
/// when an answer is missing or mistyped.
pub fn judge(
    client: &jev::Client,
    file: &str,
    text: &str,
) -> Result<Report, jev::Error> {
    let doc = parse(text);
    let mut report = Report {
        file: file.to_owned(),
        r#static: static_checks(&doc),
        judgments: None,
        acceptance: None,
        model: String::new(),
        usage: jev::Usage::default(),
    };

    if !doc.judgeable() {
        return Ok(report);
    }

    let response = client.ask(&request(&doc))?;

    let judgments = NOULS
        .into_iter()
        .map(|id| Ok((id.to_owned(), response.noul(id)?)))
        .collect::<Result<_, jev::Error>>()?;
    let (score, confidence) = response.score("acceptance_strength")?;

    report.judgments = Some(judgments);
    report.acceptance = Some(Strength { score, confidence });
    report.model = response.model;
    report.usage = response.usage;

    Ok(report)
}

#[cfg(test)]
mod tests {
    use std::{fs, path::PathBuf, time::Duration};

    use httpmock::prelude::*;

    use super::*;

    /// The check names [`static_checks`] returns, in order.
    const CHECKS: [&str; 6] = [
        "status_line",
        "sections_present",
        "sections_ordered",
        "allowed_nonempty",
        "acceptance_command",
        "budget_numeric",
    ];

    /// A response answering all three questions.
    const GOOD_RESPONSE: &str = r#"{"model":"jev-1","answers":{
        "needs_interpretation":{"type":"noul","noul":0.12},
        "acceptance_strength":{"type":"score","score":0.32,"confidence":0.81},
        "scope_generic":{"type":"noul","noul":0.05}},
        "usage":{"input_tokens":1400,"output_tokens":9}}"#;

    /// Returns the path of the tasklint fixture directory.
    fn fixtures() -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("tests/fixtures/tasklint")
    }

    /// Reads the tasklint fixture `name`.
    fn fixture(name: &str) -> String {
        fs::read_to_string(fixtures().join(name)).unwrap()
    }

    /// Returns the findings for `text` keyed by check name.
    fn findings(text: &str) -> BTreeMap<String, Finding> {
        static_checks(&parse(text))
            .into_iter()
            .map(|f| (f.check.clone(), f))
            .collect()
    }

    /// Returns a client for `server`.
    fn client(server: &MockServer) -> jev::Client {
        jev::Client::new(
            &server.base_url(),
            "sekret",
            "jev-test",
            Some(Duration::from_secs(5)),
        )
    }

    #[test]
    fn should_match_golden_for_every_fixture() {
        let golden: BTreeMap<String, Value> =
            serde_json::from_str(&fixture("golden.json")).unwrap();
        assert!(!golden.is_empty());

        let mut mismatches = Vec::new();
        for (name, record) in &golden {
            let doc = parse(&fixture(name));
            let (got, want) = if record["body"].is_null() {
                if doc.judgeable() {
                    mismatches.push(format!("{name}: judgeable"));
                }
                (
                    serde_json::to_value(static_checks(&doc)).unwrap(),
                    &record["static"],
                )
            } else {
                (
                    serde_json::to_value(request(&doc)).unwrap(),
                    &record["body"],
                )
            };
            if &got != want {
                mismatches.push(format!("{name}:\n got {got}\nwant {want}"));
            }
        }

        assert!(mismatches.is_empty(), "{}", mismatches.join("\n"));
    }

    #[test]
    fn should_report_all_six_checks_ok_for_good_01() {
        let got = static_checks(&parse(&fixture("good-01.md")));

        let names: Vec<&str> = got.iter().map(|f| f.check.as_str()).collect();
        assert_eq!(names, CHECKS);
        assert!(got.iter().all(|f| f.ok && f.detail.is_empty()), "{got:?}");
    }

    #[test]
    fn should_report_every_failure_detail() {
        let thin = "# 021 thin\nStatus: pending\n\n## Files\n\
            Read-only: a.go\n\n## Acceptance\nCommand: go test ./...\n\n\
            ## Budget\na few turns\n";
        let second = "## Files\nAllowed:\n\nRead-only: a.go\n\
            ## Acceptance\nCommand: a\nExpect: b\nCommand: c\n\
            ```\nExpect: fenced\n```\n";
        let broken = fixture("broken-01.md");

        let cases = [
            (
                thin,
                "status_line",
                r#"status "pending" is not one of ready|in-progress|done|accepted"#,
            ),
            (thin, "allowed_nonempty", "no Allowed: line"),
            (thin, "acceptance_command", "Command 1 has no Expect: line"),
            (thin, "budget_numeric", r#"no "<n> turns" in the budget"#),
            (second, "allowed_nonempty", "Allowed: names no paths"),
            (
                second,
                "acceptance_command",
                "Command 2 has no Expect: line",
            ),
            (&broken, "sections_present", "Acceptance, Rules"),
            (
                &broken,
                "sections_ordered",
                "Files appears after Out of scope",
            ),
            (&broken, "acceptance_command", "no Command: line"),
        ];

        for (text, check, detail) in cases {
            let got = &findings(text)[check];
            assert!(!got.ok, "{check} passed on {text:?}");
            assert_eq!(got.detail, detail, "{check} on {text:?}");
        }
    }

    #[test]
    fn should_pass_static_checks_as_the_go_suite_expects() {
        let cases = [
            ("good-01.md", [true; 6]),
            ("good-02.md", [true; 6]),
            ("vague-01.md", [true; 6]),
            ("vacuous-01.md", [true; 6]),
            ("generic-scope-01.md", [true; 6]),
            ("broken-01.md", [true, false, false, true, false, true]),
        ];

        for (file, want) in cases {
            let got: Vec<bool> = static_checks(&parse(&fixture(file)))
                .iter()
                .map(|f| f.ok)
                .collect();
            assert_eq!(got, want, "{file}");
        }
    }

    #[test]
    fn should_read_status_from_the_line_below_the_title() {
        for text in [
            "# test\nStatus: ready\n## Objective\nwork\n",
            "# test\n\n\nStatus: ready\n\n## Objective\nwork\n",
        ] {
            let doc = parse(text);
            assert_eq!(doc.status, "ready", "{text:?}");
            assert!(static_checks(&doc)[0].ok, "{text:?}");
        }
    }

    #[test]
    fn should_only_read_status_from_the_line_below_the_title() {
        let cases = [
            (
                "# test\n## Objective\nStatus: ready\n",
                "Objective",
                "Status: ready\n",
            ),
            (
                "# test\n## Files\nStatus: done\n",
                "Files",
                "Status: done\n",
            ),
            (
                "# test\n```\nStatus: ready\n```\n## Objective\nwork\n",
                "Objective",
                "work\n",
            ),
        ];

        for (text, section, body) in cases {
            let doc = parse(text);
            assert_eq!(doc.title, "test", "{text:?}");
            assert_eq!(doc.status, "", "{text:?}");
            assert_eq!(doc.section(section), body, "{text:?}");

            let first = &static_checks(&doc)[0];
            assert_eq!(first.check, "status_line");
            assert!(!first.ok, "{text:?}");
        }
    }

    #[test]
    fn should_ignore_fenced_headings() {
        let doc = parse(&fixture("good-02.md"));

        assert_eq!(doc.title, "007 template-doc");
        assert_eq!(doc.status, "ready");
        assert_eq!(
            doc.order.iter().filter(|s| *s == "Objective").count(),
            1,
            "{:?}",
            doc.order
        );

        let objective = doc.section("Objective");
        assert!(objective.contains("docs/task-template.md"), "{objective}");
        assert!(!objective.contains("NNN slug"), "{objective}");

        // The fenced template holds a "## Acceptance" heading and a
        // "Command: false" line before the real Acceptance section.
        let acceptance = doc.section("Acceptance");
        assert!(
            acceptance.contains("go test ./internal/tasklint/"),
            "{acceptance}"
        );
        assert!(!acceptance.contains("Command: false"), "{acceptance}");
    }

    #[test]
    fn should_keep_the_first_of_duplicate_headings() {
        let doc = parse(
            "# 020 example\nStatus: done\n\n## Files\nAllowed: a.go\n\
            ## Out of scope\nnothing\n## Files\nAllowed: b.go\n",
        );

        assert_eq!(doc.section("Files"), "Allowed: a.go\n");
        assert_eq!(
            doc.section("Out of scope"),
            "nothing\n## Files\nAllowed: b.go\n"
        );
        assert_eq!(doc.order, ["Files", "Out of scope"]);
    }

    #[test]
    fn should_return_empty_for_an_absent_section() {
        let doc = parse(&fixture("vague-01.md"));

        assert_eq!(doc.section("Spec"), "");
        assert!(doc.judgeable());
    }

    #[test]
    fn should_skip_the_model_when_sections_are_missing() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.any_request();
            then.status(200).body(GOOD_RESPONSE);
        });

        let report =
            judge(&client(&server), "broken-01.md", &fixture("broken-01.md"))
                .unwrap();

        mock.assert_calls(0);
        assert_eq!(
            serde_json::to_value(&report).unwrap(),
            json!({
                "file": "broken-01.md",
                "static": serde_json::to_value(&report.r#static).unwrap(),
                "usage": {"input_tokens": 0, "output_tokens": 0},
            })
        );
        assert_eq!(report.r#static.len(), CHECKS.len());
    }

    #[test]
    fn should_map_every_report_field() {
        let text = fixture("vacuous-01.md");
        let server = MockServer::start();
        let mut want_body =
            serde_json::to_value(request(&parse(&text))).unwrap();
        want_body["model"] = json!("jev-test");
        let mock = server.mock(|when, then| {
            when.method(POST).path(jev::PATH).json_body(want_body);
            then.status(200).body(GOOD_RESPONSE);
        });

        let report = judge(&client(&server), "vacuous-01.md", &text).unwrap();

        mock.assert();
        assert_eq!(
            report,
            Report {
                file: "vacuous-01.md".to_owned(),
                r#static: static_checks(&parse(&text)),
                judgments: Some(BTreeMap::from([
                    ("needs_interpretation".to_owned(), 0.12),
                    ("scope_generic".to_owned(), 0.05),
                ])),
                acceptance: Some(Strength {
                    score: 0.32,
                    confidence: 0.81,
                }),
                model: "jev-1".to_owned(),
                usage: jev::Usage {
                    input_tokens: 1400,
                    output_tokens: 9,
                },
            }
        );
        let wire = serde_json::to_string(&report).unwrap();
        let at: Vec<usize> = [
            "file",
            "static",
            "judgments",
            "acceptance",
            "model",
            "usage",
        ]
        .iter()
        .map(|key| wire.find(&format!("\"{key}\":")).unwrap())
        .collect();
        assert!(at.is_sorted(), "{wire}");
    }

    #[test]
    fn should_reject_a_missing_answer() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.any_request();
            then.status(200).body(
                r#"{"model":"jev-1","answers":{
                "needs_interpretation":{"type":"noul","noul":0.12},
                "scope_generic":{"type":"noul","noul":0.05}}}"#,
            );
        });

        let result =
            judge(&client(&server), "good-01.md", &fixture("good-01.md"));

        assert!(matches!(result, Err(jev::Error::Response(_))), "{result:?}");
    }
}
