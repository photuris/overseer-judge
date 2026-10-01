//! The `review` verb: parses review rounds and types each finding via Jev.
//!
//! [`parse`] splits an overseer review-round file into [`Item`]s with a
//! fence-aware line scanner, [`request`] builds the Jev request for one
//! item, and [`judge`] sends one request per item and maps the answers
//! to [`Typed`] records.

use std::{collections::BTreeMap, sync::LazyLock};

use regex::Regex;
use serde::Serialize;
use serde_json::{Value, json};

use crate::jev;

/// Instruction text for the `style_only` question.
const STYLE_INSTRUCTIONS: &str = "`finding` is one code-review finding \
    written against a task specification. Is it purely a style or \
    preference remark (naming, formatting, ordering, wording, idiom \
    choice) with no claim about correctness, behavior, the task's \
    acceptance criteria, or scope?";

/// The two outcomes the `style_only` question is judged against.
const STYLE_CRITERIA: [(&str, &str); 2] = [
    (
        "true",
        "The finding would not change what the program does or whether \
         the task's acceptance passes; it is taste.",
    ),
    (
        "false",
        "The finding claims a bug, a missing behavior, a test gap, a \
         violated acceptance criterion, or work outside the allowed \
         scope.",
    ),
];

/// The five labels a reply is classified into, each with the
/// description the model judges against.
const KIND_CRITERIA: [(&str, &str); 5] = [
    (
        "fixed",
        "Says a change was made and names where or how (a function, \
         file, commit, or test), addressing the finding.",
    ),
    (
        "evidence",
        "Disputes the finding by pointing at something checkable: a \
         code citation with a path or line, an existing test, a command \
         output, or a reproduction.",
    ),
    (
        "concern",
        "Disputes the finding by argument or opinion only, with nothing \
         checkable named.",
    ),
    (
        "question",
        "Asks the reviewer for clarification or more information \
         instead of fixing or disputing.",
    ),
    (
        "agree",
        "Accepts the finding without claiming a fix yet, e.g. 'will \
         do', 'agreed, next round'.",
    ),
];

/// Matches an item header line: `### R6-01: title`.
static HEADER: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^### (R[0-9]+-[0-9]+): (.+)$")
        .unwrap_or_else(|e| unreachable!("HEADER is a valid regex: {e}"))
});

// ── Parsing ─────────────────────────────────────────────────────────────────

/// One review finding with the implementer's responses.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Item {
    /// Finding id, such as `R6-01`.
    pub id: String,
    /// Header text after the id.
    pub title: String,
    /// The `- file:` metadata value.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub file: String,
    /// The `- severity:` metadata value.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub severity: String,
    /// The `- status:` metadata value.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub status: String,
    /// The finding text, trailing blank lines trimmed.
    pub body: String,
    /// Each response joined into one line, in document order.
    pub responses: Vec<String>,
}

/// Where the parser is within an item.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Mode {
    /// Before the first header, or after a line that ended a response:
    /// everything is ignored until the next terminator.
    Seek,
    /// Just after a header, reading `- file:`-style metadata lines.
    Meta,
    /// Reading the finding body.
    Body,
    /// Reading a response and its indented continuation lines.
    Response,
}

/// Accumulates one item at a time as [`parse`] walks the file.
struct Parser<'a> {
    /// Finished items, in document order.
    items: Vec<Item>,
    /// The item in progress.
    cur: Option<Item>,
    /// Body lines of the item in progress.
    body: Vec<&'a str>,
    /// Trimmed lines of the response in progress.
    resp: Vec<&'a str>,
    /// Where the parser is within the item in progress.
    mode: Mode,
    /// Whether the item in progress has had a metadata line, which
    /// decides what a blank line in [`Mode::Meta`] means.
    saw_meta: bool,
}

/// Extracts items in document order; a file with no items yields an
/// empty `Vec`.
///
/// Every `\r` is removed first, so CRLF input parses as LF. An item
/// starts at a `### R<n>-<nn>: title` header outside a fenced block,
/// takes the `- file:`, `- severity:`, and `- status:` lines that
/// follow it (blank lines before the block are skipped; once begun the
/// block is contiguous), then a body that runs to the first
/// `- response:`, the next header, a `---` line, or EOF. A response
/// continues through lines indented two spaces, with blank lines
/// allowed when the next line is indented; a line that ends a response
/// without being a terminator is ignored, as is everything up to the
/// next terminator.
pub fn parse(text: &str) -> Vec<Item> {
    let text = text.replace('\r', "");
    let lines: Vec<&str> = text
        .strip_suffix('\n')
        .unwrap_or(&text)
        .split('\n')
        .collect();

    let mut p = Parser {
        items: Vec::new(),
        cur: None,
        body: Vec::new(),
        resp: Vec::new(),
        mode: Mode::Seek,
        saw_meta: false,
    };
    let mut fenced = false;
    for (i, &line) in lines.iter().enumerate() {
        let next = lines.get(i + 1).copied().unwrap_or("");

        // A fence marker, and everything inside a fence, is content:
        // never a header, a terminator, or a response.
        if line.starts_with("```") {
            fenced = !fenced;
            p.content(line, next);
        } else if fenced {
            p.content(line, next);
        } else {
            p.structure(line, next);
        }
    }
    p.flush();

    p.items
}

impl<'a> Parser<'a> {
    /// Handles one line outside a fence, where headers, metadata,
    /// terminators, and response openers are recognised.
    fn structure(&mut self, line: &'a str, next: &str) {
        if let Some(caps) = HEADER.captures(line) {
            self.start_item(&caps[1], &caps[2]);
        } else if let Some((key, value)) =
            meta(line).filter(|_| self.mode == Mode::Meta)
        {
            self.meta(key, value);
        } else if line == "---" {
            self.flush();
        } else if let Some(first) = line
            .strip_prefix("- response:")
            .filter(|_| self.cur.is_some())
        {
            self.start_response(first);
        } else {
            self.content(line, next);
        }
    }

    /// Accumulates a line that is not a header, a terminator, or a
    /// response opener. `next` is the following line, which decides
    /// whether a blank line continues a response.
    fn content(&mut self, line: &'a str, next: &str) {
        let blank = line.trim().is_empty();
        match self.mode {
            Mode::Seek => {}
            // Blank lines between the header and the metadata are
            // skipped. Once a metadata line has been read the block is
            // contiguous, so the next blank line closes it.
            Mode::Meta if blank => {
                if self.saw_meta {
                    self.mode = Mode::Body;
                }
            }
            Mode::Meta | Mode::Body => {
                self.mode = Mode::Body;
                self.body.push(line);
            }
            Mode::Response if line.starts_with("  ") => {
                if !blank {
                    self.resp.push(line.trim());
                }
            }
            Mode::Response if blank && next.starts_with("  ") => {}
            Mode::Response => {
                self.end_response();
                self.mode = Mode::Seek;
            }
        }
    }

    /// Finishes the item in progress and opens a new one.
    fn start_item(&mut self, id: &str, title: &str) {
        self.flush();
        self.cur = Some(Item {
            id: id.to_owned(),
            title: title.to_owned(),
            file: String::new(),
            severity: String::new(),
            status: String::new(),
            body: String::new(),
            responses: Vec::new(),
        });
        self.mode = Mode::Meta;
        self.saw_meta = false;
    }

    /// Records one metadata line of the item in progress.
    fn meta(&mut self, key: &str, value: &str) {
        self.saw_meta = true;

        let Some(cur) = self.cur.as_mut() else { return };
        let field = match key {
            "file" => &mut cur.file,
            "severity" => &mut cur.severity,
            _ => &mut cur.status,
        };
        value.trim().clone_into(field);
    }

    /// Finishes the response in progress and opens another from the
    /// text following `- response:`.
    fn start_response(&mut self, first: &'a str) {
        self.end_response();
        self.mode = Mode::Response;
        self.resp.clear();

        let first = first.trim();
        if !first.is_empty() {
            self.resp.push(first);
        }
    }

    /// Stores the response in progress, if there is one, as one line.
    fn end_response(&mut self) {
        if self.mode != Mode::Response {
            return;
        }

        if let Some(cur) = self.cur.as_mut() {
            cur.responses.push(self.resp.join(" ").trim().to_owned());
        }
        self.resp.clear();
    }

    /// Finishes the item in progress, trimming trailing blank lines
    /// from its body, and appends it to the result.
    fn flush(&mut self) {
        self.end_response();

        if let Some(mut cur) = self.cur.take() {
            let end = self
                .body
                .iter()
                .rposition(|line| !line.trim().is_empty())
                .map_or(0, |last| last + 1);
            cur.body = self.body[..end].join("\n");
            self.items.push(cur);
        }
        self.body.clear();
        self.mode = Mode::Seek;
    }
}

/// Splits a `- file|severity|status: value` line into key and raw
/// value.
fn meta(line: &str) -> Option<(&str, &str)> {
    let (key, value) = line.strip_prefix("- ")?.split_once(": ")?;

    matches!(key, "file" | "severity" | "status").then_some((key, value))
}

// ── Requests ────────────────────────────────────────────────────────────────

/// One reply's classification.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct TypedResponse {
    /// One of the five labels in `KIND_CRITERIA`.
    pub kind: String,
    /// How confident the model is in `kind`.
    pub confidence: f64,
    /// Probability per label.
    pub probabilities: BTreeMap<String, f64>,
}

/// One item's judgment.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Typed {
    /// The item's id.
    pub id: String,
    /// The item's severity, omitted when empty.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub severity: String,
    /// Probability that the finding is purely a style remark.
    pub style_only: f64,
    /// One classification per response, in order.
    pub responses: Vec<TypedResponse>,
    /// The model that answered.
    pub model: String,
    /// Tokens the request consumed.
    pub usage: jev::Usage,
}

/// Returns the question id for the `n`th response.
fn response_id(n: usize) -> String {
    format!("response_{n}")
}

/// Turns `(label, description)` pairs into a JSON object.
fn criteria(pairs: &[(&str, &str)]) -> Value {
    pairs
        .iter()
        .map(|&(label, text)| (label.to_owned(), Value::from(text)))
        .collect::<serde_json::Map<_, _>>()
        .into()
}

/// Returns the question set for `item`: `style_only` (a noul), plus
/// `response_N` (a choice of five labels) for each response, N from 0.
pub fn questions(item: &Item) -> BTreeMap<String, jev::Question> {
    let style = jev::Question {
        r#type: "noul".into(),
        instructions: STYLE_INSTRUCTIONS.into(),
        criteria: Some(criteria(&STYLE_CRITERIA)),
    };
    let replies = (0..item.responses.len()).map(|n| {
        let question = jev::Question {
            r#type: "choice".into(),
            instructions: format!(
                "`responses[{n}]` is the implementer's reply to \
                 `finding`. Classify the reply by what it does, not by \
                 whether it is right."
            ),
            criteria: Some(criteria(&KIND_CRITERIA)),
        };
        (response_id(n), question)
    });

    std::iter::once(("style_only".to_owned(), style))
        .chain(replies)
        .collect()
}

/// Builds the request state for `item`. `file` and `severity` are
/// present even when empty; `status` is never sent.
pub fn state(item: &Item) -> Value {
    json!({
        "finding": {
            "id": item.id,
            "title": item.title,
            "file": item.file,
            "severity": item.severity,
            "body": item.body,
        },
        "responses": item.responses,
    })
}

/// Returns the request [`judge_item`] sends for `item`. The model is
/// left `None` for the client or the caller to fill.
pub fn request(item: &Item) -> jev::Request {
    jev::Request {
        state: state(item),
        model: None,
        questions: questions(item),
    }
}

/// Asks the questions for one item and maps the answers to a
/// [`Typed`].
///
/// # Errors
///
/// Any [`jev::Error`] from the call, and [`jev::Error::Response`] when
/// an answer breaks its contract, including a kind outside the five
/// labels.
pub fn judge_item(
    client: &jev::Client,
    item: &Item,
) -> Result<Typed, jev::Error> {
    let resp = client.ask(&request(item))?;
    let style_only = resp.noul("style_only")?;

    let responses = (0..item.responses.len())
        .map(|n| {
            let qid = response_id(n);
            let (kind, confidence, probabilities) = resp.choice(&qid)?;
            if !KIND_CRITERIA.iter().any(|&(label, _)| label == kind) {
                return Err(jev::Error::Response(format!(
                    "answer \"{qid}\" chose \"{kind}\", not one of the \
                     five labels"
                )));
            }

            Ok(TypedResponse {
                kind: kind.to_owned(),
                confidence,
                probabilities: probabilities.clone(),
            })
        })
        .collect::<Result<_, _>>()?;

    Ok(Typed {
        id: item.id.clone(),
        severity: item.severity.clone(),
        style_only,
        responses,
        model: resp.model,
        usage: resp.usage,
    })
}

/// Sends one request per item, sequentially, and returns the results
/// in input order.
///
/// # Errors
///
/// The first error from [`judge_item`], which aborts the rest.
pub fn judge(
    client: &jev::Client,
    items: &[Item],
) -> Result<Vec<Typed>, jev::Error> {
    items.iter().map(|item| judge_item(client, item)).collect()
}

#[cfg(test)]
mod tests {
    use std::{fs, path::PathBuf, time::Duration};

    use httpmock::prelude::*;

    use super::*;

    /// Reads the review fixture `name`.
    fn fixture(name: &str) -> String {
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("tests/fixtures/review")
            .join(name);
        fs::read_to_string(path).unwrap()
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

    /// Builds a response body whose `response_0` chose `kind`.
    fn answer(kind: &str) -> String {
        format!(
            r#"{{"model":"jev-1","answers":{{
            "style_only":{{"type":"noul","noul":0.12}},
            "response_0":{{"type":"choice","choice":"{kind}",
            "confidence":0.77,"probabilities":{{"{kind}":0.77,"concern":0.23}}}}}},
            "usage":{{"input_tokens":900,"output_tokens":6}}}}"#
        )
    }

    /// Builds an item with `id`, `severity`, and `responses`.
    fn item(id: &str, severity: &str, responses: &[&str]) -> Item {
        Item {
            id: id.into(),
            title: String::new(),
            file: String::new(),
            severity: severity.into(),
            status: String::new(),
            body: String::new(),
            responses: responses.iter().map(|&r| r.to_owned()).collect(),
        }
    }

    #[test]
    fn should_match_golden_for_every_fixture() {
        let golden: BTreeMap<String, Value> =
            serde_json::from_str(&fixture("golden.json")).unwrap();
        assert_eq!(golden.len(), 3, "golden holds {} fixtures", golden.len());

        let mut mismatches = Vec::new();
        for (name, want) in &golden {
            let got: Vec<Value> = parse(&fixture(name))
                .iter()
                .map(request)
                .map(|r| serde_json::to_value(r).unwrap())
                .collect();
            let Some(want) = want.as_array() else {
                mismatches.push(format!("{name}: golden is not an array"));
                continue;
            };
            if got.len() != want.len() {
                mismatches.push(format!(
                    "{name}: {} requests, want {}",
                    got.len(),
                    want.len()
                ));
            }
            for (i, (g, w)) in got.iter().zip(want).enumerate() {
                if g != w {
                    mismatches
                        .push(format!("{name}[{i}]:\n got {g}\nwant {w}"));
                }
            }
        }
        assert!(mismatches.is_empty(), "{}", mismatches.join("\n"));
    }

    #[test]
    fn should_parse_round_one() {
        let items = parse(&fixture("round-01.md"));
        let ids: Vec<&str> = items.iter().map(|it| it.id.as_str()).collect();
        assert_eq!(
            ids,
            ["R6-01", "R6-02", "R6-03", "R6-04", "R6-05", "R6-06"]
        );
        let counts: Vec<usize> =
            items.iter().map(|it| it.responses.len()).collect();
        assert_eq!(counts, [1, 1, 1, 1, 1, 2]);

        let first = &items[0];
        assert_eq!(
            first.title,
            "Pending re-run can start a batch while the mapping editor is \
             open"
        );
        assert_eq!(first.file, "src/shell/controller.cpp:1055");
        assert_eq!(first.severity, "medium");
        assert_eq!(first.status, "resolved");
        assert!(first.responses[0].contains("closeEditor()"));
        assert!(!first.responses[0].contains('\n'));

        for it in &items {
            assert!(
                !it.body.contains("- response:"),
                "{}: {}",
                it.id,
                it.body
            );
        }

        // R6-03's fence holds a real header line and a real terminator.
        let body: Vec<&str> = items[2].body.split('\n').collect();
        assert!(
            body.contains(&"### R9-99: not an item"),
            "{}",
            items[2].body
        );
        assert!(body.contains(&"---"), "{}", items[2].body);
        // The response quotes an indented fenced snippet, which must not
        // toggle the fence flag and must stay inside the response.
        assert!(items[2].responses[0].contains("```cpp"));
        assert!(items[2].responses[0].contains("break;"));

        assert!(!items[5].responses[1].contains("overseer note"));
        assert!(items[5].responses[0].contains("dead code"));
    }

    #[test]
    fn should_parse_round_two() {
        let items = parse(&fixture("round-02.md"));
        let ids: Vec<&str> = items.iter().map(|it| it.id.as_str()).collect();
        assert_eq!(ids, ["R1-01", "R1-02", "R1-03"]);
        assert!(items[1].responses.is_empty(), "{:?}", items[1].responses);
        for it in &items {
            assert!(!it.body.contains("Round 1 of task 004"), "{}", it.id);
        }
    }

    #[test]
    fn should_parse_round_three_with_blank_line_before_metadata() {
        let items = parse(&fixture("round-03.md"));
        assert_eq!(items.len(), 6, "{items:#?}");

        let severities = [
            "blocking", "blocking", "minor", "blocking", "blocking",
            "blocking",
        ];
        for (i, it) in items.iter().enumerate() {
            assert_eq!(it.id, format!("R1-{:02}", i + 1));
            assert_eq!(it.severity, severities[i], "{}", it.id);
            assert_eq!(it.status, "resolved", "{}", it.id);
            assert!(
                it.file.starts_with("internal/"),
                "{}: {}",
                it.id,
                it.file
            );
            assert!(!it.body.contains("- severity:"), "{}", it.id);
            assert_eq!(it.responses.len(), 1, "{}", it.id);
        }
    }

    #[test]
    fn should_yield_no_item_for_fenced_template() {
        for it in parse(&fixture("round-03.md")) {
            assert!(!it.title.contains("one-line title"), "{}", it.id);
        }
    }

    #[test]
    fn should_skip_blanks_before_metadata_and_close_on_blank_after() {
        let items = parse(
            "### R1-01: t\n\n\n- file: a.go:1\n- severity: minor\n\n\
             - status: open\nbody\n",
        );
        assert_eq!(items.len(), 1);
        assert_eq!(items[0].file, "a.go:1");
        assert_eq!(items[0].severity, "minor");
        assert_eq!(items[0].status, "");
        assert_eq!(items[0].body, "- status: open\nbody");
    }

    #[test]
    fn should_parse_crlf_like_lf() {
        let lf = "### R1-01: t\nbody\n---\n- response: outside\n";
        let want = parse(lf);
        assert_eq!(
            want,
            [item("R1-01", "", &[])].map(|it| Item {
                title: "t".into(),
                body: "body".into(),
                ..it
            })
        );
        assert_eq!(parse(&lf.replace('\n', "\r\n")), want);

        let text = fixture("round-01.md");
        let crlf = parse(&text.replace('\n', "\r\n"));
        assert_eq!(crlf, parse(&text));
        for it in &crlf {
            let all = format!(
                "{}{}{}{}{}{}",
                it.title,
                it.file,
                it.severity,
                it.status,
                it.body,
                it.responses.concat()
            );
            assert!(!all.contains('\r'), "{}", it.id);
        }
    }

    #[test]
    fn should_return_no_items_for_empty_input() {
        assert!(parse("").is_empty());
    }

    #[test]
    fn should_ask_one_question_per_response() {
        let qs = questions(&item("R1-01", "", &["a", "b"]));
        let types: BTreeMap<&str, &str> = qs
            .iter()
            .map(|(id, q)| (id.as_str(), q.r#type.as_str()))
            .collect();
        assert_eq!(
            types,
            BTreeMap::from([
                ("response_0", "choice"),
                ("response_1", "choice"),
                ("style_only", "noul"),
            ])
        );
        assert!(qs["response_1"].instructions.contains("responses[1]"));
    }

    #[test]
    fn should_send_one_request_per_item() {
        let server = MockServer::start();
        let first = server.mock(|when, then| {
            when.method(POST).path(jev::PATH).body_includes("\"R1-01\"");
            then.status(200).body(answer("fixed"));
        });
        let second = server.mock(|when, then| {
            when.method(POST).path(jev::PATH).body_includes("\"R1-02\"");
            then.status(200).body(answer("evidence"));
        });

        let typed = judge(
            &client(&server),
            &[item("R1-01", "high", &["a"]), item("R1-02", "low", &["b"])],
        )
        .unwrap();

        first.assert_calls(1);
        second.assert_calls(1);
        let got: Vec<(&str, &str)> = typed
            .iter()
            .map(|t| (t.id.as_str(), t.responses[0].kind.as_str()))
            .collect();
        assert_eq!(got, [("R1-01", "fixed"), ("R1-02", "evidence")]);

        let head = &typed[0];
        assert_eq!(head.severity, "high");
        assert_eq!(head.style_only, 0.12);
        assert_eq!(head.model, "jev-1");
        assert_eq!(head.usage.input_tokens, 900);
        assert_eq!(head.responses[0].confidence, 0.77);
        assert_eq!(head.responses[0].probabilities["fixed"], 0.77);
    }

    #[test]
    fn should_reject_unknown_kind() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(POST).path(jev::PATH);
            then.status(200).body(answer("maybe"));
        });

        let err =
            judge(&client(&server), &[item("R1-01", "", &["a"])]).unwrap_err();
        match err {
            jev::Error::Response(message) => assert_eq!(
                message,
                "answer \"response_0\" chose \"maybe\", not one of the \
                 five labels"
            ),
            other => panic!("want Error::Response, got {other:?}"),
        }
    }

    #[test]
    fn should_ask_only_style_when_no_responses() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.method(POST)
                .path(jev::PATH)
                .body_includes("\"style_only\"")
                .body_excludes("\"response_0\"");
            then.status(200).body(
                r#"{"model":"jev-1","answers":{"style_only":
                {"type":"noul","noul":0.88}},
                "usage":{"input_tokens":5,"output_tokens":1}}"#,
            );
        });

        let item = item("R1-02", "", &[]);
        assert_eq!(request(&item).questions.len(), 1);
        let typed = judge(&client(&server), &[item]).unwrap();

        mock.assert_calls(1);
        assert!(typed[0].responses.is_empty());
    }

    #[test]
    fn should_serialize_typed_in_field_order() {
        let typed = Typed {
            id: "R1-01".into(),
            severity: String::new(),
            style_only: 0.5,
            responses: Vec::new(),
            model: "m".into(),
            usage: jev::Usage::default(),
        };
        assert_eq!(
            serde_json::to_string(&typed).unwrap(),
            r#"{"id":"R1-01","style_only":0.5,"responses":[],"model":"m","usage":{"input_tokens":0,"output_tokens":0}}"#
        );
    }
}
