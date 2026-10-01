//! The `session` verb: classifies an agent pane capture into a session
//! state via Jev.
//!
//! [`request`] cleans the pane tail, reads the input box and busy
//! indicator out of it, and asks two questions: which of six states the
//! pane is in, and whether its latest output is coherent. [`judge`]
//! sends that request and maps the answers to a [`Verdict`].

use std::{collections::BTreeMap, fmt, str::FromStr};

use serde::Serialize;
use serde_json::{Value, json};

pub use self::clean::{
    MAX_BYTES, MAX_LINES, activity_hint, clean, input_line,
};
use crate::jev;

mod clean;

/// The confidence a verdict must reach before a caller acts on it
/// unattended; below it, escalate to a person or a reasoning model.
/// Nothing here enforces it.
pub const GATE: f64 = 0.9;

/// Instructions for the `state` question.
const STATE_INSTRUCTIONS: &str = "`transcript_tail` is the most recent \
    terminal output of a coding agent (`agent_kind`) running in a \
    terminal pane. `input_line` is the text currently sitting in the \
    agent's input box, extracted by code (empty string if the box is \
    empty or was not found). `activity_hint` is the agent's own \
    busy-indicator line as found on screen by code, for example a \
    spinner or progress bar beside 'esc to interrupt'; it is an empty \
    string when no busy indicator is on screen. Classify what the pane \
    is doing right now, judged by the last few screens of output, giving \
    most weight to the final lines.";

/// Instructions for the `coherent` question.
const COHERENT_INSTRUCTIONS: &str = "Is the most recent output in \
    `transcript_tail` coherent, on-task natural language or code, as \
    opposed to repetition loops, mixed-language gibberish, or random \
    tokens?";

/// The six labels the `state` question chooses between, each with the
/// description the model judges against.
const STATE_CRITERIA: [(&str, &str); 6] = [
    (
        "working",
        "The agent is busy right now and its output is coherent. \
         `activity_hint` is non-empty, showing the agent's own busy \
         indicator; a busy indicator with no output yet still counts. \
         When `activity_hint` is empty, choose this only if the final \
         lines show a tool call or command still in progress. Finished \
         output above an input box is not working, and text in \
         `input_line` does not mean the agent is working.",
    ),
    (
        "idle",
        "The agent has finished and is waiting for input. \
         `activity_hint` is empty, `input_line` is empty or holds only \
         the tool's placeholder hint (for example 'Ask Codex to do \
         anything'), and there is no dialog.",
    ),
    (
        "dialog",
        "A modal UI that blocks the agent until a person responds: an \
         approval or permission prompt, a folder-trust prompt, a \
         numbered or yes/no choice under a cursor, or a usage-limit or \
         rate-limit screen that must be dismissed before work can \
         continue. Informational banners, warnings, update notices, and \
         tips that do not wait for a keypress are not dialogs.",
    ),
    (
        "unsubmitted",
        "`input_line` holds text a person typed (an instruction, \
         question, or partial message, not the tool's placeholder hint) \
         and `activity_hint` is empty, so the agent is not acting on it. \
         The text has been typed but not sent, however much finished \
         output sits above it.",
    ),
    (
        "error",
        "The agent's work ended with an API error, crash, stack trace, \
         connection failure, or process exit, and it is not continuing.",
    ),
    (
        "degraded",
        "The output has become incoherent: the same line or phrase \
         repeating many times, mixed-language token salad, random \
         characters, or text that no longer relates to any task. This \
         holds even when `activity_hint` shows a busy indicator, because \
         a degraded agent keeps producing.",
    ),
];

/// The two outcomes the `coherent` question is judged against.
const COHERENT_CRITERIA: [(&str, &str); 2] = [
    (
        "true",
        "The latest output reads as purposeful text or code a competent \
         engineer would write.",
    ),
    (
        "false",
        "The latest output is repetitive, garbled, multilingual salad, or \
         otherwise meaningless.",
    ),
];

/// The coding agent whose pane is judged; it selects how the input box
/// and busy indicator are found.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AgentKind {
    /// Claude Code.
    Claude,
    /// OpenAI Codex CLI.
    Codex,
    /// pi.
    Pi,
    /// opencode.
    Opencode,
    /// Not known: every extraction strategy is tried.
    Unknown,
}

impl AgentKind {
    /// Returns the kind's wire name, as sent in the request state.
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Claude => "claude",
            Self::Codex => "codex",
            Self::Pi => "pi",
            Self::Opencode => "opencode",
            Self::Unknown => "unknown",
        }
    }
}

/// A string that names none of the [`AgentKind`]s.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UnknownAgentKind(pub String);

impl fmt::Display for UnknownAgentKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "unknown agent kind {:?}", self.0)
    }
}

impl std::error::Error for UnknownAgentKind {}

impl FromStr for AgentKind {
    type Err = UnknownAgentKind;

    /// Parses one of the five wire names returned by
    /// [`AgentKind::as_str`].
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "claude" => Ok(Self::Claude),
            "codex" => Ok(Self::Codex),
            "pi" => Ok(Self::Pi),
            "opencode" => Ok(Self::Opencode),
            "unknown" => Ok(Self::Unknown),
            _ => Err(UnknownAgentKind(s.to_owned())),
        }
    }
}

/// The session judgment.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Verdict {
    /// The chosen state: one of the six labels.
    pub state: String,
    /// The model's confidence in `state`.
    pub confidence: f64,
    /// Probability per state label.
    pub probabilities: BTreeMap<String, f64>,
    /// Probability that the latest output is coherent.
    pub coherent: f64,
    /// The text found in the agent's input box.
    pub input_line: String,
    /// The agent's busy-indicator line found on screen.
    pub activity_hint: String,
    /// The model that answered.
    pub model: String,
    /// Tokens the request consumed.
    pub usage: jev::Usage,
}

/// Returns the two questions sent for every session judgment: `state`
/// (a choice of six labels) and `coherent` (a noul).
pub fn questions() -> BTreeMap<String, jev::Question> {
    BTreeMap::from([
        (
            "state".to_owned(),
            jev::Question {
                r#type: "choice".into(),
                instructions: STATE_INSTRUCTIONS.into(),
                criteria: Some(criteria(&STATE_CRITERIA)),
            },
        ),
        (
            "coherent".to_owned(),
            jev::Question {
                r#type: "noul".into(),
                instructions: COHERENT_INSTRUCTIONS.into(),
                criteria: Some(criteria(&COHERENT_CRITERIA)),
            },
        ),
    ])
}

/// Turns `(label, description)` pairs into a JSON object.
fn criteria(pairs: &[(&str, &str)]) -> Value {
    pairs
        .iter()
        .map(|&(label, text)| (label.to_owned(), Value::from(text)))
        .collect::<serde_json::Map<_, _>>()
        .into()
}

/// Builds the request state for a `cleaned` tail, with the input line
/// and activity hint read from it.
pub fn state(kind: AgentKind, cleaned: &str) -> Value {
    json!({
        "agent_kind": kind.as_str(),
        "transcript_tail": cleaned,
        "input_line": input_line(kind, cleaned),
        "activity_hint": activity_hint(kind, cleaned),
    })
}

/// Returns the request [`judge`] sends for `raw_tail`, cleaned with
/// [`MAX_LINES`] and [`MAX_BYTES`]. The model is left `None` for the
/// client or the caller to fill.
pub fn request(kind: AgentKind, raw_tail: &str) -> jev::Request {
    request_for(kind, &clean(raw_tail, MAX_LINES, MAX_BYTES))
}

/// Builds the request for an already-cleaned tail.
fn request_for(kind: AgentKind, cleaned: &str) -> jev::Request {
    jev::Request {
        state: state(kind, cleaned),
        model: None,
        questions: questions(),
    }
}

/// Cleans `raw_tail` once, asks the two questions, and maps the answers
/// to a [`Verdict`].
///
/// # Errors
///
/// Any [`jev::Error`] from the call, and [`jev::Error::Response`] when
/// an answer breaks its contract, including a `state` label outside the
/// six.
pub fn judge(
    client: &jev::Client,
    kind: AgentKind,
    raw_tail: &str,
) -> Result<Verdict, jev::Error> {
    let cleaned = clean(raw_tail, MAX_LINES, MAX_BYTES);
    let resp = client.ask(&request_for(kind, &cleaned))?;
    let (state, confidence, probabilities) = resp.choice("state")?;

    if !STATE_CRITERIA.iter().any(|&(label, _)| label == state) {
        return Err(jev::Error::Response(format!(
            "answer \"state\" chose {state:?}, not one of the six labels"
        )));
    }

    let coherent = resp.noul("coherent")?;

    Ok(Verdict {
        state: state.to_owned(),
        confidence,
        probabilities: probabilities.clone(),
        coherent,
        input_line: input_line(kind, &cleaned),
        activity_hint: activity_hint(kind, &cleaned),
        model: resp.model.clone(),
        usage: resp.usage.clone(),
    })
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use httpmock::prelude::*;

    use super::*;

    /// Every pane fixture, by file name.
    const FIXTURES: [(&str, &str); 24] = [
        (
            "ambiguous-01.txt",
            include_str!("../tests/fixtures/session/ambiguous-01.txt"),
        ),
        (
            "degraded-01.txt",
            include_str!("../tests/fixtures/session/degraded-01.txt"),
        ),
        (
            "degraded-02.txt",
            include_str!("../tests/fixtures/session/degraded-02.txt"),
        ),
        (
            "dialog-01.txt",
            include_str!("../tests/fixtures/session/dialog-01.txt"),
        ),
        (
            "dialog-02.txt",
            include_str!("../tests/fixtures/session/dialog-02.txt"),
        ),
        (
            "error-01.txt",
            include_str!("../tests/fixtures/session/error-01.txt"),
        ),
        (
            "error-02.txt",
            include_str!("../tests/fixtures/session/error-02.txt"),
        ),
        (
            "idle-01.txt",
            include_str!("../tests/fixtures/session/idle-01.txt"),
        ),
        (
            "idle-02.txt",
            include_str!("../tests/fixtures/session/idle-02.txt"),
        ),
        (
            "idle-03.txt",
            include_str!("../tests/fixtures/session/idle-03.txt"),
        ),
        (
            "idle-04.txt",
            include_str!("../tests/fixtures/session/idle-04.txt"),
        ),
        (
            "idle-05.txt",
            include_str!("../tests/fixtures/session/idle-05.txt"),
        ),
        (
            "idle-06.txt",
            include_str!("../tests/fixtures/session/idle-06.txt"),
        ),
        (
            "idle-07.txt",
            include_str!("../tests/fixtures/session/idle-07.txt"),
        ),
        (
            "idle-08.txt",
            include_str!("../tests/fixtures/session/idle-08.txt"),
        ),
        (
            "unsubmitted-02.txt",
            include_str!("../tests/fixtures/session/unsubmitted-02.txt"),
        ),
        (
            "unsubmitted-03.txt",
            include_str!("../tests/fixtures/session/unsubmitted-03.txt"),
        ),
        (
            "unsubmitted-04.txt",
            include_str!("../tests/fixtures/session/unsubmitted-04.txt"),
        ),
        (
            "unsubmitted-05.txt",
            include_str!("../tests/fixtures/session/unsubmitted-05.txt"),
        ),
        (
            "working-01.txt",
            include_str!("../tests/fixtures/session/working-01.txt"),
        ),
        (
            "working-02.txt",
            include_str!("../tests/fixtures/session/working-02.txt"),
        ),
        (
            "working-03.txt",
            include_str!("../tests/fixtures/session/working-03.txt"),
        ),
        (
            "working-04.txt",
            include_str!("../tests/fixtures/session/working-04.txt"),
        ),
        (
            "working-05.txt",
            include_str!("../tests/fixtures/session/working-05.txt"),
        ),
    ];

    /// The Go request body, model removed, per `"<fixture>/<agent>"`.
    const GOLDEN: &str = include_str!("../tests/fixtures/session/golden.json");

    /// A complete, well-formed answer pair.
    const GOOD_RESPONSE: &str = r#"{"model":"jev-1","answers":{
        "state":{"type":"choice","choice":"idle","confidence":0.91,
            "probabilities":{"idle":0.91,"working":0.09}},
        "coherent":{"type":"noul","noul":0.97}},
        "usage":{"input_tokens":1200,"output_tokens":8}}"#;

    /// Returns a client for `server` that answers every request with
    /// `body`.
    fn client_answering(server: &MockServer, body: &str) -> jev::Client {
        server.mock(|when, then| {
            when.method(POST).path(jev::PATH);
            then.status(200).body(body);
        });

        jev::Client::new(
            &server.base_url(),
            "sekret",
            "jev-test",
            Some(Duration::from_secs(5)),
        )
    }

    #[test]
    fn should_match_golden_request_for_every_fixture_and_kind() {
        let golden: BTreeMap<String, Value> =
            serde_json::from_str(GOLDEN).unwrap();
        let fixtures = BTreeMap::from(FIXTURES);

        let failures: Vec<&str> = golden
            .iter()
            .filter(|(key, want)| {
                let (fixture, kind) = key.rsplit_once('/').unwrap();
                let kind: AgentKind = kind.parse().unwrap();
                let got =
                    serde_json::to_value(request(kind, fixtures[fixture]))
                        .unwrap();

                got != **want
            })
            .map(|(key, _)| key.as_str())
            .collect();

        assert_eq!(golden.len(), 120, "golden holds {} cases", golden.len());
        assert!(failures.is_empty(), "mismatching keys: {failures:?}");
    }

    #[test]
    fn should_round_trip_agent_kind_names() {
        let kinds = [
            AgentKind::Claude,
            AgentKind::Codex,
            AgentKind::Pi,
            AgentKind::Opencode,
            AgentKind::Unknown,
        ];

        for kind in kinds {
            assert_eq!(kind.as_str().parse::<AgentKind>(), Ok(kind));
        }
        assert_eq!(
            "Claude".parse::<AgentKind>(),
            Err(UnknownAgentKind("Claude".into()))
        );
    }

    /// Returns pi's labelled rule line: `──`, `label`, then twenty `─`.
    fn pi_rule(label: &str) -> String {
        format!("──{label}{}", "─".repeat(20))
    }

    /// Returns what pi's `activity_hint` and `input_line` read from a
    /// pane whose rule above `typed` carries `label`.
    fn pi_reads(label: &str) -> (String, String) {
        let rule = pi_rule(label);
        let pane = format!("{rule}\ntyped\n{}", pi_rule(""));

        (
            activity_hint(AgentKind::Pi, &rule),
            input_line(AgentKind::Pi, &pane),
        )
    }

    #[test]
    fn should_reject_pi_rule_label_at_25_chars() {
        for (len, is_rule) in [(24, true), (25, false), (26, false)] {
            let label = "a".repeat(len);

            let (hint, input) = pi_reads(&label);

            let want = if is_rule {
                (label.as_str(), "typed")
            } else {
                ("", "")
            };
            assert_eq!((hint.as_str(), input.as_str()), want, "len {len}");
        }
    }

    #[test]
    fn should_use_go_letter_and_digit_classes_for_pi_labels() {
        let cases = [
            ("²", false),
            ("\u{345}", false),
            ("Ⅻ", false),
            ("é", true),
            ("٣", true),
        ];

        for (label, is_rule) in cases {
            let (hint, input) = pi_reads(&format!(" {label} "));

            let want = if is_rule { (label, "typed") } else { ("", "") };
            assert_eq!((hint.as_str(), input.as_str()), want, "{label:?}");
        }
    }

    #[test]
    fn should_reject_unknown_state_label() {
        let server = MockServer::start();
        let client = client_answering(
            &server,
            r#"{"model":"m","answers":{"state":{"type":"choice",
                "choice":"banana","confidence":0.9,
                "probabilities":{"banana":0.9}},
                "coherent":{"type":"noul","noul":0.9}}}"#,
        );

        let err = judge(&client, AgentKind::Unknown, "tail").unwrap_err();

        let jev::Error::Response(message) = err else {
            panic!("want Error::Response, got {err:?}");
        };
        assert_eq!(
            message,
            r#"answer "state" chose "banana", not one of the six labels"#
        );
    }

    #[test]
    fn should_map_every_verdict_field() {
        let server = MockServer::start();
        let client = client_answering(&server, GOOD_RESPONSE);

        let verdict = judge(
            &client,
            AgentKind::Claude,
            "done\r\n❯ ship it\x1b[2m and the ghost\x1b[0m\n\
             ✶ Wrangling… (esc to interrupt)",
        )
        .unwrap();

        assert_eq!(
            verdict,
            Verdict {
                state: "idle".into(),
                confidence: 0.91,
                probabilities: BTreeMap::from([
                    ("idle".into(), 0.91),
                    ("working".into(), 0.09),
                ]),
                coherent: 0.97,
                input_line: "ship it".into(),
                activity_hint: "✶ Wrangling… (esc to interrupt)".into(),
                model: "jev-1".into(),
                usage: jev::Usage {
                    input_tokens: 1200,
                    output_tokens: 8,
                },
            }
        );
    }

    #[test]
    fn should_serialize_verdict_fields_in_order() {
        let server = MockServer::start();
        let client = client_answering(&server, GOOD_RESPONSE);
        let verdict = judge(&client, AgentKind::Claude, "❯ x").unwrap();

        let text = serde_json::to_string(&verdict).unwrap();

        assert_eq!(
            text,
            r#"{"state":"idle","confidence":0.91,"probabilities":{"idle":0.91,"working":0.09},"coherent":0.97,"input_line":"x","activity_hint":"","model":"jev-1","usage":{"input_tokens":1200,"output_tokens":8}}"#
        );
    }
}
