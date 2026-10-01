//! Client for TypeSafe's System One (Jev) API.
//!
//! [`Client`] posts a [`Request`] to `{base_url}/v1/systemone`,
//! retries rate limits and server errors, and decodes the reply into a
//! [`Response`] whose accessors return typed answers. Every diagnostic
//! the client copies from a response has the API key redacted.

use std::{
    collections::BTreeMap,
    io::Read,
    thread,
    time::{Duration, Instant},
};

use serde::{Deserialize, Deserializer, Serialize};
use serde_json::{Value, value::RawValue};
use ureq::{
    Agent, AsSendBody, Body,
    http::{self, Uri, header},
};

/// The System One endpoint, relative to the base URL.
pub const PATH: &str = "/v1/systemone";

/// Waits before the second and third attempts, unless the server sends
/// a `Retry-After`.
const RETRY_WAITS: [Duration; 2] =
    [Duration::from_millis(500), Duration::from_secs(1)];

/// How many bytes of an error response body are kept as its message.
const MAX_ERR_BODY: u64 = 500;

/// How many redirects one attempt follows before giving up.
const MAX_REDIRECTS: u32 = 10;

/// Replaces the API key wherever a diagnostic echoes it.
const REDACTED: &str = "[redacted]";

// ── Wire types ──────────────────────────────────────────────────────────────

/// One System One question.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Question {
    /// Answer type: `noul`, `choice`, or `score`.
    pub r#type: String,
    /// What the model should judge.
    pub instructions: String,
    /// Type-specific criteria, such as the labels of a `choice`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub criteria: Option<Value>,
}

/// A System One request body.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Request {
    /// The application state the questions are asked about.
    pub state: Value,
    /// Model to ask; [`Client::ask`] fills the client default when
    /// `None`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    /// The questions, keyed by the id their answers come back under.
    pub questions: BTreeMap<String, Question>,
}

/// The tokens a request consumed.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Default)]
pub struct Usage {
    /// Tokens read; `null` or absent decodes as 0.
    #[serde(default, deserialize_with = "null_default")]
    pub input_tokens: u64,
    /// Tokens written; `null` or absent decodes as 0.
    #[serde(default, deserialize_with = "null_default")]
    pub output_tokens: u64,
}

/// One answer from a System One response.
///
/// Which fields are set depends on [`Answer::type`]; use the
/// [`Response`] accessors to read them with validation.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Default)]
pub struct Answer {
    /// Answer type: `noul`, `choice`, or `score`; `null` or absent
    /// decodes as empty.
    #[serde(default, deserialize_with = "null_default")]
    pub r#type: String,
    /// Probability that the answer is yes, for a `noul` answer.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub noul: Option<f64>,
    /// The chosen label, for a `choice` answer.
    #[serde(default, skip_serializing_if = "is_none_or_empty_str")]
    pub choice: Option<String>,
    /// Probability per label, for a `choice` answer; a `null`
    /// probability decodes as 0.
    #[serde(
        default,
        deserialize_with = "null_probabilities_zero",
        skip_serializing_if = "is_none_or_empty_map"
    )]
    pub probabilities: Option<BTreeMap<String, f64>>,
    /// The score, for a `score` answer.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub score: Option<f64>,
    /// Description per label or score point.
    #[serde(default, skip_serializing_if = "is_none_or_empty_map")]
    pub legend: Option<BTreeMap<String, Value>>,
    /// How confident the model is in the answer.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub confidence: Option<f64>,
}

/// A System One response body.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub struct Response {
    /// The model that answered; `null` or absent decodes as empty.
    #[serde(default, deserialize_with = "null_default")]
    pub model: String,
    /// The answers, keyed by question id. A `null` answer decodes as
    /// [`Answer::default`].
    #[serde(deserialize_with = "null_values_default")]
    pub answers: BTreeMap<String, Answer>,
    /// Token usage; `null` or absent decodes as zeros.
    #[serde(default, deserialize_with = "null_default")]
    pub usage: Usage,
}

/// Serde helper: decodes `null` (or, with `#[serde(default)]`, an
/// absent field) as `T::default()`.
fn null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// Serde helper: decodes an answers map whose `null` values become
/// [`Answer::default`].
fn null_values_default<'de, D>(
    d: D,
) -> Result<BTreeMap<String, Answer>, D::Error>
where
    D: Deserializer<'de>,
{
    let answers = BTreeMap::<String, Option<Answer>>::deserialize(d)?;

    Ok(answers
        .into_iter()
        .map(|(id, answer)| (id, answer.unwrap_or_default()))
        .collect())
}

/// Serde helper: decodes a probabilities map whose `null` values
/// become 0, as Go's `map[string]float64` does.
fn null_probabilities_zero<'de, D>(
    d: D,
) -> Result<Option<BTreeMap<String, f64>>, D::Error>
where
    D: Deserializer<'de>,
{
    let probabilities =
        Option::<BTreeMap<String, Option<f64>>>::deserialize(d)?;

    Ok(probabilities.map(|probabilities| {
        probabilities
            .into_iter()
            .map(|(label, p)| (label, p.unwrap_or_default()))
            .collect()
    }))
}

/// Serde skip predicate: true for `None` and `Some("")`.
fn is_none_or_empty_str(value: &Option<String>) -> bool {
    value.as_deref().is_none_or(str::is_empty)
}

/// Serde skip predicate: true for `None` and an empty map.
fn is_none_or_empty_map<V>(value: &Option<BTreeMap<String, V>>) -> bool {
    value.as_ref().is_none_or(BTreeMap::is_empty)
}

// ── Typed answers ───────────────────────────────────────────────────────────

impl Response {
    /// Returns the probability of the `noul` answer `id`.
    ///
    /// # Errors
    ///
    /// Returns [`Error::Response`] when the answer is missing, is not
    /// of type `noul`, or has no `noul` value.
    pub fn noul(&self, id: &str) -> Result<f64, Error> {
        let answer = self.answer(id, "noul")?;

        answer.noul.ok_or_else(|| invalid(id, "has no noul value"))
    }

    /// Returns `(score, confidence)` of the `score` answer `id`.
    ///
    /// # Errors
    ///
    /// Returns [`Error::Response`] when the answer is missing, is not
    /// of type `score`, or lacks a score or a confidence.
    pub fn score(&self, id: &str) -> Result<(f64, f64), Error> {
        let answer = self.answer(id, "score")?;

        let score = answer
            .score
            .ok_or_else(|| invalid(id, "has no score value"))?;
        let confidence = answer
            .confidence
            .ok_or_else(|| invalid(id, "has no confidence"))?;

        Ok((score, confidence))
    }

    /// Returns `(choice, confidence, probabilities)` of the `choice`
    /// answer `id`.
    ///
    /// # Errors
    ///
    /// Returns [`Error::Response`] when the answer is missing, is not
    /// of type `choice`, lacks a non-empty choice, a confidence, or
    /// probabilities, or chose a label absent from its probabilities.
    pub fn choice(
        &self,
        id: &str,
    ) -> Result<(&str, f64, &BTreeMap<String, f64>), Error> {
        let answer = self.answer(id, "choice")?;

        let choice = answer
            .choice
            .as_deref()
            .filter(|choice| !choice.is_empty())
            .ok_or_else(|| invalid(id, "has no choice"))?;
        let confidence = answer
            .confidence
            .ok_or_else(|| invalid(id, "has no confidence"))?;
        let probabilities = answer
            .probabilities
            .as_ref()
            .ok_or_else(|| invalid(id, "has no probabilities"))?;

        if !probabilities.contains_key(choice) {
            return Err(invalid(
                id,
                &format!("chose {choice:?}, absent from probabilities"),
            ));
        }

        Ok((choice, confidence, probabilities))
    }

    /// Returns the answer `id`, checking that it exists and has type
    /// `want`.
    fn answer(&self, id: &str, want: &str) -> Result<&Answer, Error> {
        let answer =
            self.answers.get(id).ok_or_else(|| invalid(id, "missing"))?;

        if answer.r#type != want {
            return Err(invalid(
                id,
                &format!("has type {:?}, want {want:?}", answer.r#type),
            ));
        }

        Ok(answer)
    }
}

/// Builds the [`Error::Response`] for answer `id` with `problem`.
fn invalid(id: &str, problem: &str) -> Error {
    Error::Response(format!("answer {id:?} {problem}"))
}

// ── Errors ──────────────────────────────────────────────────────────────────

/// A failed System One call.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The API rejected the credentials (401).
    #[error("authentication failed (status {status})")]
    Auth {
        /// The HTTP status.
        status: u16,
    },
    /// A 429 that survived the client's retries.
    #[error("rate limited (status {status})")]
    RateLimit {
        /// The HTTP status.
        status: u16,
    },
    /// A 5xx that survived the client's retries.
    #[error("upstream error (status {status}): {message}")]
    Server {
        /// The HTTP status.
        status: u16,
        /// The first bytes of the response body, key redacted.
        message: String,
    },
    /// Any other non-200 status: the request was rejected.
    #[error("request rejected (status {status}): {message}")]
    Request {
        /// The HTTP status.
        status: u16,
        /// The first bytes of the response body, key redacted.
        message: String,
    },
    /// A 200 response that violates the answer contract.
    #[error("invalid response: {0}")]
    Response(String),
    /// A transport failure or timeout.
    #[error("network failure: {0}")]
    Network(String),
}

/// Why one attempt failed, and whether another is worth making.
enum Failure {
    /// Another attempt may succeed; `after` is the server's
    /// `Retry-After`, and `error` is reported if retries run out.
    Retry {
        /// The error to report once retries run out.
        error: Error,
        /// The wait the server asked for, if any.
        after: Option<Duration>,
    },
    /// Retrying cannot help.
    Final(Error),
}

impl Failure {
    /// Returns the error to report for this failure.
    fn into_error(self) -> Error {
        match self {
            Self::Retry { error, .. } | Self::Final(error) => error,
        }
    }
}

// ── Client ──────────────────────────────────────────────────────────────────

/// Sends System One requests over its own HTTP agent.
// No `Debug` derive: `api_key` must never reach a log line.
pub struct Client {
    /// HTTP agent with the configured timeout; redirects are followed
    /// by [`Client::follow`], not by the agent.
    agent: Agent,
    /// Bound on one attempt, redirects included; `None` is unbounded.
    timeout: Option<Duration>,
    /// Full endpoint URL: base URL plus [`PATH`].
    url: String,
    /// Bearer token sent with every request.
    api_key: String,
    /// Model filled into requests that name none.
    model: String,
}

impl Client {
    /// Returns a client posting to `base_url` with `api_key`, filling
    /// `model` into requests that name none.
    ///
    /// `timeout` bounds each attempt end to end; `None` disables it.
    pub fn new(
        base_url: &str,
        api_key: &str,
        model: &str,
        timeout: Option<Duration>,
    ) -> Self {
        let config = Agent::config_builder()
            .timeout_global(timeout)
            .http_status_as_error(false)
            .max_redirects(0)
            .max_redirects_will_error(false)
            .build();

        Self {
            agent: Agent::new_with_config(config),
            timeout,
            url: format!("{base_url}{PATH}"),
            api_key: api_key.to_owned(),
            model: model.to_owned(),
        }
    }

    /// Serializes `req`, filling the client's model when it names
    /// none, and sends it with [`Client::ask_raw`].
    ///
    /// # Errors
    ///
    /// As [`Client::ask_raw`].
    ///
    /// # Panics
    ///
    /// Never in practice: serializing a [`Request`] cannot fail, since
    /// it holds only strings, string-keyed maps, and JSON values.
    pub fn ask(&self, req: &Request) -> Result<Response, Error> {
        /// The wire form of a [`Request`] with its model filled in.
        #[derive(Serialize)]
        struct Outgoing<'a> {
            /// See [`Request::state`].
            state: &'a Value,
            /// See [`Request::model`].
            model: &'a str,
            /// See [`Request::questions`].
            questions: &'a BTreeMap<String, Question>,
        }

        let outgoing = Outgoing {
            state: &req.state,
            model: req.model.as_deref().unwrap_or(&self.model),
            questions: &req.questions,
        };
        let body = serde_json::to_vec(&outgoing).unwrap_or_else(|err| {
            unreachable!("strings and JSON values always serialize: {err}")
        });

        self.ask_raw(&body)
    }

    /// POSTs `body` verbatim and decodes the response.
    ///
    /// Status 429 and every 5xx are retried, three attempts in total,
    /// waiting 500 ms then 1 s, or the server's `Retry-After` seconds.
    /// Redirects are followed as Go's HTTP client does (see
    /// [`Client::follow`]).
    ///
    /// # Errors
    ///
    /// [`Error::Auth`] on 401; [`Error::RateLimit`] or [`Error::Server`]
    /// when a 429 or 5xx persists; [`Error::Request`] on any other
    /// non-200 status; [`Error::Response`] when a 200 body is not a
    /// valid response; [`Error::Network`] on a transport failure or
    /// timeout.
    pub fn ask_raw(&self, body: &[u8]) -> Result<Response, Error> {
        let mut outcome = self.attempt(body, 1);

        for (attempt, default_wait) in (2..).zip(RETRY_WAITS) {
            let Err(Failure::Retry { after, .. }) = &outcome else {
                break;
            };

            thread::sleep(after.unwrap_or(default_wait));
            outcome = self.attempt(body, attempt);
        }

        outcome.map_err(Failure::into_error)
    }

    /// Performs attempt number `attempt` and classifies its outcome.
    fn attempt(&self, body: &[u8], attempt: u32) -> Result<Response, Failure> {
        tracing::debug!(attempt, bytes = body.len(), "posting to System One");

        let mut resp = self.follow(body).map_err(Failure::Final)?;
        let status = resp.status().as_u16();

        tracing::debug!(attempt, status, "System One replied");

        if status == 200 {
            let bytes = resp
                .body_mut()
                .with_config()
                .limit(u64::MAX)
                .read_to_vec()
                .map_err(network)?;

            tracing::debug!(attempt, bytes = bytes.len(), "read response");

            return decode(&bytes).map_err(|msg| {
                Failure::Final(Error::Response(self.redact(&msg)))
            });
        }

        let message = self.redact(&error_body(resp.body_mut()));

        Err(match status {
            401 => Failure::Final(Error::Auth { status }),
            429 => Failure::Retry {
                error: Error::RateLimit { status },
                after: retry_after(resp.headers()),
            },
            500.. => Failure::Retry {
                error: Error::Server { status, message },
                after: retry_after(resp.headers()),
            },
            _ => Failure::Final(Error::Request { status, message }),
        })
    }

    /// POSTs `body` to the endpoint and returns the response that ends
    /// its redirect chain.
    ///
    /// At most [`MAX_REDIRECTS`] redirects are followed. 301, 302, and
    /// 303 turn the request into a GET without body; 307 and 308 resend
    /// it unchanged. `Authorization` goes only to the endpoint's host
    /// and port.
    ///
    /// # Errors
    ///
    /// [`Error::Network`] on a transport failure, a timeout of the
    /// whole chain, a missing or invalid `Location`, or too many
    /// redirects.
    fn follow(&self, body: &[u8]) -> Result<http::Response<Body>, Error> {
        let origin = Uri::try_from(self.url.as_str())
            .map_err(|err| Error::Network(err.to_string()))?;
        let deadline = self.timeout.map(|timeout| Instant::now() + timeout);
        let mut url = origin.clone();
        let mut body = Some(body);

        for followed in 0..=MAX_REDIRECTS {
            let resp = self
                .send(&url, body, same_host(&origin, &url), deadline)
                .map_err(|err| Error::Network(err.to_string()))?;
            let status = resp.status().as_u16();

            if !matches!(status, 301 | 302 | 303 | 307 | 308) {
                return Ok(resp);
            }

            if followed == MAX_REDIRECTS {
                break;
            }

            tracing::debug!(status, hop = followed + 1, "following redirect");

            url = resp
                .headers()
                .get(header::LOCATION)
                .and_then(|location| location.to_str().ok())
                .and_then(|location| resolve(&url, location))
                .ok_or_else(|| {
                    Error::Network(format!(
                        "status {status} without a valid Location"
                    ))
                })?;

            if status <= 303 {
                body = None;
            }
        }

        Err(Error::Network(format!(
            "stopped after {MAX_REDIRECTS} redirects"
        )))
    }

    /// Sends one request of a redirect chain: a JSON POST of `body`,
    /// or a GET when it is `None`, authorized when `authorize` is set,
    /// with whatever remains of the time until `deadline`.
    fn send(
        &self,
        url: &Uri,
        body: Option<&[u8]>,
        authorize: bool,
        deadline: Option<Instant>,
    ) -> Result<http::Response<Body>, ureq::Error> {
        let mut req = http::Request::builder().uri(url.clone());

        if authorize {
            req = req.header(
                header::AUTHORIZATION,
                format!("Bearer {}", self.api_key),
            );
        }

        let remaining = deadline.map(|deadline| {
            deadline.saturating_duration_since(Instant::now())
        });

        match body {
            Some(body) => self.run(
                req.method(http::Method::POST)
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(body)?,
                remaining,
            ),
            None => {
                self.run(req.method(http::Method::GET).body(())?, remaining)
            }
        }
    }

    /// Runs `req` on the agent with `timeout` as its global timeout.
    fn run(
        &self,
        req: http::Request<impl AsSendBody>,
        timeout: Option<Duration>,
    ) -> Result<http::Response<Body>, ureq::Error> {
        let req = self
            .agent
            .configure_request(req)
            .timeout_global(timeout)
            .build();

        self.agent.run(req)
    }

    /// Replaces every occurrence of the API key in `text`.
    fn redact(&self, text: &str) -> String {
        if self.api_key.is_empty() {
            return text.to_owned();
        }

        text.replace(&self.api_key, REDACTED)
    }
}

/// Reports whether `url` has the same host and port as `origin`.
fn same_host(origin: &Uri, url: &Uri) -> bool {
    let hosts_match = origin
        .host()
        .zip(url.host())
        .is_some_and(|(a, b)| a.eq_ignore_ascii_case(b));

    hosts_match && origin.port_u16() == url.port_u16()
}

/// Resolves a redirect `location` against the URL that returned it.
///
/// Returns `None` when `location` is empty or the result is not a
/// valid URI.
// ponytail: no dot-segment removal; `../x` is sent as is, which servers
// normalize. Add RFC 3986 section 5.2.4 if a server ever needs it.
fn resolve(base: &Uri, location: &str) -> Option<Uri> {
    let location = location.split('#').next().unwrap_or_default();
    let scheme = base.scheme_str()?;
    let authority = base.authority()?;
    let path = base.path();

    let resolved = if location.is_empty() {
        return None;
    } else if location.starts_with("//") {
        format!("{scheme}:{location}")
    } else if location.starts_with('/') {
        format!("{scheme}://{authority}{location}")
    } else if location.starts_with('?') {
        format!("{scheme}://{authority}{path}{location}")
    } else if Uri::try_from(location).is_ok_and(|uri| uri.scheme().is_some()) {
        location.to_owned()
    } else {
        let dir = &path[..=path.rfind('/').unwrap_or_default()];

        format!("{scheme}://{authority}{dir}{location}")
    };

    Uri::try_from(resolved).ok()
}

/// Wraps a transport error as a final [`Error::Network`].
fn network(err: ureq::Error) -> Failure {
    Failure::Final(Error::Network(err.to_string()))
}

/// Reads at most [`MAX_ERR_BODY`] bytes of an error response body.
///
/// A read failure yields an empty message: the status alone still
/// classifies the error.
fn error_body(body: &mut Body) -> String {
    let mut bytes = Vec::new();

    if body
        .as_reader()
        .take(MAX_ERR_BODY)
        .read_to_end(&mut bytes)
        .is_err()
    {
        return String::new();
    }

    String::from_utf8_lossy(&bytes).into_owned()
}

/// Parses a `Retry-After` header given as integer seconds, as Go's
/// `strconv.Atoi` reads it; a negative value is ignored, `-0` is zero.
fn retry_after(headers: &http::HeaderMap) -> Option<Duration> {
    let secs: i64 = headers.get("Retry-After")?.to_str().ok()?.parse().ok()?;

    u64::try_from(secs).ok().map(Duration::from_secs)
}

/// Decodes a 200 body into a [`Response`], or the reason it is invalid.
///
/// The body must be one JSON object with a non-null object `answers`.
/// Every check reads the raw bytes, so members the [`Response`] ignores
/// are never materialized and cannot fail the decode.
fn decode(body: &[u8]) -> Result<Response, String> {
    /// The body's `answers` member, unparsed.
    #[derive(Deserialize)]
    struct Envelope<'a> {
        /// The raw `answers` value, `null` included; `None` if absent.
        #[serde(borrow, default, deserialize_with = "present")]
        answers: Option<&'a RawValue>,
    }

    let decode_err = |err: serde_json::Error| format!("decode body: {err}");

    // Validates the syntax and rejects trailing values without building
    // a tree; `get` excludes the surrounding whitespace.
    let raw: &RawValue = serde_json::from_slice(body).map_err(decode_err)?;
    let text = raw.get();

    if text == "null" {
        return Err("body is null".into());
    }

    if !text.starts_with('{') {
        return Err("body is not a JSON object".into());
    }

    let envelope: Envelope = serde_json::from_str(text).map_err(decode_err)?;

    let Some(answers) = envelope.answers else {
        return Err(r#"body has no "answers""#.into());
    };

    if answers.get() == "null" {
        return Err(r#""answers" is null"#.into());
    }

    if !answers.get().starts_with('{') {
        return Err(r#""answers" is not an object"#.into());
    }

    serde_json::from_str(text).map_err(decode_err)
}

/// Serde helper: wraps a present raw value, `null` included, in `Some`,
/// where a plain `Option` would turn `null` into `None`.
fn present<'de, D>(d: D) -> Result<Option<&'de RawValue>, D::Error>
where
    D: Deserializer<'de>,
{
    <&RawValue>::deserialize(d).map(Some)
}

#[cfg(test)]
mod tests {
    use std::{
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
        time::Instant,
    };

    use httpmock::prelude::*;
    use rstest::rstest;
    use serde_json::json;

    use super::*;

    /// The API key every test client sends.
    const KEY: &str = "sekret-key";

    /// A valid response body with one answer of each type.
    const OK_BODY: &str = r#"{
        "model": "jev-latest",
        "answers": {
            "n": {"type": "noul", "noul": 0.75},
            "c": {
                "type": "choice",
                "choice": "idle",
                "probabilities": {"idle": 0.8, "working": 0.2},
                "confidence": 0.8
            },
            "s": {"type": "score", "score": 3.5, "confidence": 0.6}
        },
        "usage": {"input_tokens": 12, "output_tokens": 3}
    }"#;

    /// Returns a client for `server` with a 5 s timeout.
    fn client(server: &MockServer) -> Client {
        Client::new(
            &server.base_url(),
            KEY,
            "jev-default",
            Some(Duration::from_secs(5)),
        )
    }

    /// Serves `status` with `body` to every request on `server`.
    fn serve<'a>(
        server: &'a MockServer,
        status: u16,
        body: &str,
    ) -> httpmock::Mock<'a> {
        server.mock(|when, then| {
            when.method(POST).path(PATH);
            then.status(status).body(body);
        })
    }

    /// Returns a request with one noul question and the given model.
    fn request(model: Option<&str>) -> Request {
        Request {
            state: json!("x"),
            model: model.map(str::to_owned),
            questions: BTreeMap::from([(
                "n".to_owned(),
                Question {
                    r#type: "noul".into(),
                    instructions: "is it?".into(),
                    criteria: None,
                },
            )]),
        }
    }

    /// Decodes `body` as a 200 response through a real client.
    fn decode_ok(body: &str) -> Result<Response, Error> {
        let server = MockServer::start();
        serve(&server, 200, body);

        client(&server).ask_raw(b"{}")
    }

    /// Returns the message of an [`Error::Response`], panicking on any
    /// other result.
    fn response_message(result: Result<Response, Error>) -> String {
        match result {
            Err(Error::Response(message)) => message,
            other => panic!("want Error::Response, got {other:?}"),
        }
    }

    #[test]
    fn should_send_body_verbatim_with_auth_headers() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.method(POST)
                .path(PATH)
                .header("authorization", format!("Bearer {KEY}"))
                .header("content-type", "application/json")
                .body(r#"{"state":"x"}"#);
            then.status(200).body(OK_BODY);
        });

        let resp = client(&server).ask_raw(br#"{"state":"x"}"#).unwrap();

        mock.assert();
        assert_eq!(resp.usage.input_tokens, 12, "usage = {:?}", resp.usage);
    }

    #[test]
    fn should_fill_model_when_request_has_none() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.method(POST).path(PATH).json_body(json!({
                "state": "x",
                "model": "jev-default",
                "questions": {"n": {"type": "noul", "instructions": "is it?"}}
            }));
            then.status(200).body(OK_BODY);
        });

        client(&server).ask(&request(None)).unwrap();

        mock.assert();
    }

    #[test]
    fn should_keep_model_when_request_names_one() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.method(POST)
                .path(PATH)
                .json_body_includes(r#"{"model": "explicit"}"#);
            then.status(200).body(OK_BODY);
        });

        client(&server).ask(&request(Some("explicit"))).unwrap();

        mock.assert();
    }

    #[test]
    fn should_map_401_to_auth() {
        let server = MockServer::start();
        serve(&server, 401, "nope");

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        assert!(matches!(err, Error::Auth { status: 401 }), "err = {err:?}");
    }

    #[test]
    fn should_map_422_to_request_with_body_message() {
        let server = MockServer::start();
        serve(&server, 422, "bad question");

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        assert_eq!(
            err.to_string(),
            "request rejected (status 422): bad question"
        );
    }

    #[test]
    fn should_truncate_error_message_to_500_bytes() {
        let server = MockServer::start();
        serve(&server, 400, &"x".repeat(600));

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        let Error::Request { message, .. } = err else {
            panic!("want Error::Request, got {err:?}");
        };
        assert_eq!(message.len(), 500);
    }

    #[test]
    fn should_retry_twice_then_report_rate_limit_when_429_persists() {
        let server = MockServer::start();
        let mock = serve(&server, 429, "slow down");

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        mock.assert_calls(3);
        assert!(
            matches!(err, Error::RateLimit { status: 429 }),
            "err = {err:?}"
        );
    }

    #[test]
    fn should_retry_twice_then_report_server_when_500_persists() {
        let server = MockServer::start();
        let mock = serve(&server, 500, "boom");

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        mock.assert_calls(3);
        assert_eq!(err.to_string(), "upstream error (status 500): boom");
    }

    #[test]
    fn should_succeed_on_third_attempt_after_two_503s() {
        let server = MockServer::start();
        let seen = Arc::new(AtomicUsize::new(0));
        let failing = server.mock(|when, then| {
            let seen = Arc::clone(&seen);
            when.method(POST)
                .path(PATH)
                .is_true(move |_| seen.fetch_add(1, Ordering::SeqCst) < 2);
            then.status(503).header("Retry-After", "0").body("busy");
        });
        let ok = serve(&server, 200, OK_BODY);

        let resp = client(&server).ask_raw(b"{}").unwrap();

        failing.assert_calls(2);
        ok.assert_calls(1);
        assert_eq!(resp.model, "jev-latest");
    }

    #[test]
    fn should_not_retry_when_request_is_rejected() {
        let server = MockServer::start();
        let mock = serve(&server, 404, "no such path");

        client(&server).ask_raw(b"{}").unwrap_err();

        mock.assert_calls(1);
    }

    #[test]
    fn should_honor_retry_after_header() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.method(POST).path(PATH);
            then.status(503).header("Retry-After", "0").body("busy");
        });
        let started = Instant::now();

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        let elapsed = started.elapsed();
        mock.assert_calls(3);
        assert!(matches!(err, Error::Server { status: 503, .. }));
        assert!(
            elapsed < Duration::from_millis(400),
            "elapsed = {elapsed:?}"
        );
    }

    #[test]
    fn should_report_network_when_host_is_unreachable() {
        let client = Client::new(
            "http://127.0.0.1:1",
            KEY,
            "jev-default",
            Some(Duration::from_secs(5)),
        );

        let err = client.ask_raw(b"{}").unwrap_err();

        assert!(matches!(err, Error::Network(_)), "err = {err:?}");
    }

    #[test]
    fn should_reject_invalid_response_bodies() {
        let cases = [
            ("[1]", "body is not a JSON object"),
            ("null", "body is null"),
            ("{}", r#"body has no "answers""#),
            (r#"{"answers":null}"#, r#""answers" is null"#),
            (r#"{"answers":1}"#, r#""answers" is not an object"#),
            (
                "{} {}",
                "decode body: trailing characters at line 1 column 4",
            ),
            ("nonsense", "decode body: expected ident at line 1 column 2"),
        ];

        for (body, want) in cases {
            assert_eq!(
                response_message(decode_ok(body)),
                want,
                "body = {body}"
            );
        }
    }

    #[test]
    fn should_redact_echoed_key_in_request_error() {
        let server = MockServer::start();
        serve(&server, 400, &format!("bad key {KEY}, really {KEY}"));

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        assert_eq!(
            err.to_string(),
            "request rejected (status 400): bad key [redacted], really \
             [redacted]"
        );
    }

    #[test]
    fn should_redact_echoed_key_in_response_error() {
        let body = format!(r#"{{"answers":{{"q":{{"legend":"{KEY}"}}}}}}"#);

        let message = response_message(decode_ok(&body));

        assert!(
            message.contains(REDACTED) && !message.contains(KEY),
            "message = {message}"
        );
    }

    #[test]
    fn should_not_treat_201_as_success() {
        let server = MockServer::start();
        serve(&server, 201, OK_BODY);

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        assert!(
            matches!(err, Error::Request { status: 201, .. }),
            "err = {err:?}"
        );
    }

    #[test]
    fn should_default_missing_and_null_fields() {
        let zero_usage = r#""usage":{"input_tokens":0,"output_tokens":0}"#;
        let cases = [
            (
                r#"{"answers":{}}"#,
                format!(r#"{{"model":"","answers":{{}},{zero_usage}}}"#),
            ),
            (
                r#"{"model":null,"answers":{"q":null},"usage":null}"#,
                format!(
                    r#"{{"model":"","answers":{{"q":{{"type":""}}}},{zero_usage}}}"#
                ),
            ),
            (
                r#"{"answers":{},"usage":{"input_tokens":7}}"#,
                r#"{"model":"","answers":{},"usage":{"input_tokens":7,"output_tokens":0}}"#
                    .to_owned(),
            ),
        ];

        for (body, want) in cases {
            let resp = decode_ok(body).unwrap();

            assert_eq!(serde_json::to_string(&resp).unwrap(), want);
        }
    }

    #[test]
    fn should_serialize_answer_fields_in_order() {
        let body = r#"{"answers":{"q":{"confidence":0.5,"legend":{"a":"A"},
            "score":2.0,"probabilities":{"a":1.0},"choice":"a",
            "noul":0.1,"type":"t","extra":true}}}"#;

        let resp = decode_ok(body).unwrap();

        assert_eq!(
            serde_json::to_string(&resp.answers["q"]).unwrap(),
            r#"{"type":"t","noul":0.1,"choice":"a","probabilities":{"a":1.0},"score":2.0,"legend":{"a":"A"},"confidence":0.5}"#
        );
    }

    #[test]
    fn should_omit_empty_probabilities_and_legend() {
        let body = r#"{"answers":{"q":{"type":"choice","choice":"",
            "probabilities":{},"legend":{}}}}"#;

        let resp = decode_ok(body).unwrap();

        assert_eq!(
            serde_json::to_string(&resp.answers["q"]).unwrap(),
            r#"{"type":"choice"}"#
        );
    }

    #[test]
    fn should_reject_non_object_legend() {
        let body = r#"{"answers":{"q":{"type":"choice","legend":"x"}}}"#;

        let message = response_message(decode_ok(body));

        assert!(message.starts_with("decode body:"), "message = {message}");
    }

    #[test]
    fn should_disable_timeout_when_none() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(POST).path(PATH);
            then.status(200)
                .body(OK_BODY)
                .delay(Duration::from_millis(1500));
        });
        let url = server.base_url();
        let untimed = Client::new(&url, KEY, "m", None);
        let timed =
            Client::new(&url, KEY, "m", Some(Duration::from_millis(200)));

        let untimed_result = untimed.ask_raw(b"{}");
        let timed_result = timed.ask_raw(b"{}");

        assert!(untimed_result.is_ok(), "untimed = {untimed_result:?}");
        assert!(
            matches!(timed_result, Err(Error::Network(_))),
            "timed = {timed_result:?}"
        );
    }

    #[test]
    fn should_follow_307_and_308_replaying_the_body() {
        for status in [307, 308] {
            let server = MockServer::start();
            server.mock(|when, then| {
                when.method(POST).path(PATH);
                then.status(status).header("Location", "/final");
            });
            let replayed = server.mock(|when, then| {
                when.method(POST)
                    .path("/final")
                    .header("authorization", format!("Bearer {KEY}"))
                    .header("content-type", "application/json")
                    .body(r#"{"state":"x"}"#);
                then.status(200).body(OK_BODY);
            });

            let result = client(&server).ask_raw(br#"{"state":"x"}"#);

            assert!(result.is_ok(), "status {status}: {result:?}");
            replayed.assert();
        }
    }

    #[test]
    fn should_keep_auth_on_same_host_redirect_and_drop_it_cross_host() {
        let server = MockServer::start();
        let other = MockServer::start();
        server.mock(|when, then| {
            when.method(POST).path(PATH).body("same");
            then.status(302).header("Location", "/final");
        });
        server.mock(|when, then| {
            when.method(POST).path(PATH).body("cross");
            then.status(302)
                .header("Location", format!("{}/final", other.base_url()));
        });
        let same = server.mock(|when, then| {
            when.method(GET)
                .path("/final")
                .header("authorization", format!("Bearer {KEY}"))
                .header_missing("content-type");
            then.status(200).body(OK_BODY);
        });
        let cross = other.mock(|when, then| {
            when.method(GET)
                .path("/final")
                .header_missing("authorization")
                .header_missing("content-type");
            then.status(200).body(OK_BODY);
        });

        let same_result = client(&server).ask_raw(b"same");
        let cross_result = client(&server).ask_raw(b"cross");

        assert!(same_result.is_ok(), "same = {same_result:?}");
        assert!(cross_result.is_ok(), "cross = {cross_result:?}");
        same.assert();
        cross.assert();
    }

    #[test]
    fn should_stop_after_ten_redirects() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.method(POST).path(PATH);
            then.status(307).header("Location", PATH);
        });

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        mock.assert_calls(11);
        assert_eq!(
            err.to_string(),
            "network failure: stopped after 10 redirects"
        );
    }

    #[test]
    fn should_report_network_when_location_is_missing() {
        let server = MockServer::start();
        serve(&server, 302, "");

        let err = client(&server).ask_raw(b"{}").unwrap_err();

        assert!(matches!(err, Error::Network(_)), "err = {err:?}");
    }

    #[rstest]
    #[case::absolute("http://b:2/x?y", "http://b:2/x?y")]
    #[case::scheme_relative("//b/x", "http://b/x")]
    #[case::root_relative("/x?y#z", "http://a:1/x?y")]
    #[case::query("?y", "http://a:1/v1/systemone?y")]
    #[case::relative("final", "http://a:1/v1/final")]
    fn should_resolve_redirect_location(
        #[case] location: &str,
        #[case] want: &str,
    ) {
        let base = Uri::from_static("http://a:1/v1/systemone?q");

        let resolved = resolve(&base, location).map(|uri| uri.to_string());

        assert_eq!(resolved.as_deref(), Some(want));
    }

    #[test]
    fn should_decode_null_probability_as_zero() {
        let body = r#"{"answers":{"q":{"type":"choice","choice":"a",
            "confidence":1,"probabilities":{"a":null}}}}"#;

        let resp = decode_ok(body).unwrap();

        let (_, _, probabilities) = resp.choice("q").unwrap();
        assert_eq!(probabilities, &BTreeMap::from([("a".to_owned(), 0.0)]));
    }

    #[test]
    fn should_read_bodies_over_ten_mib() {
        let filler = "x".repeat(11 << 20);
        let body = format!(r#"{{"answers":{{}},"ignored":"{filler}"}}"#);

        let result = decode_ok(&body);

        assert!(result.is_ok(), "err = {:?}", result.err());
    }

    #[test]
    fn should_ignore_unparseable_unknown_fields() {
        let result = decode_ok(r#"{"answers":{},"ignored":1e400}"#);

        assert!(result.is_ok(), "result = {result:?}");
    }

    #[test]
    fn should_reject_duplicate_typed_fields() {
        let body =
            r#"{"answers":{"q":{"type":"noul","noul":"bad","noul":1}}}"#;

        let message = response_message(decode_ok(body));

        assert!(message.starts_with("decode body:"), "message = {message}");
    }

    #[test]
    fn should_treat_retry_after_negative_zero_as_zero() {
        let server = MockServer::start();
        let mock = server.mock(|when, then| {
            when.method(POST).path(PATH);
            then.status(503).header("Retry-After", "-0").body("busy");
        });
        let started = Instant::now();

        client(&server).ask_raw(b"{}").unwrap_err();

        let elapsed = started.elapsed();
        mock.assert_calls(3);
        assert!(
            elapsed < Duration::from_millis(400),
            "elapsed = {elapsed:?}"
        );
    }

    #[test]
    fn should_read_typed_answers_when_valid() {
        let resp = decode_ok(OK_BODY).unwrap();

        let (choice, confidence, probabilities) = resp.choice("c").unwrap();

        assert_eq!(resp.noul("n").unwrap(), 0.75);
        assert_eq!(resp.score("s").unwrap(), (3.5, 0.6));
        assert_eq!((choice, confidence), ("idle", 0.8));
        assert_eq!(probabilities["working"], 0.2);
    }

    #[rstest]
    #[case::missing(r#"{}"#, r#"answer "q" missing"#)]
    #[case::wrong_type(
        r#"{"q":{"type":"score"}}"#,
        r#"answer "q" has type "score", want "noul""#
    )]
    #[case::no_value(
        r#"{"q":{"type":"noul"}}"#,
        r#"answer "q" has no noul value"#
    )]
    fn should_reject_invalid_noul_answer(
        #[case] answers: &str,
        #[case] want: &str,
    ) {
        let resp = parsed(answers);

        let err = resp.noul("q").unwrap_err();

        assert_eq!(err.to_string(), format!("invalid response: {want}"));
    }

    #[rstest]
    #[case::missing(r#"{}"#, r#"answer "q" missing"#)]
    #[case::wrong_type(
        r#"{"q":{"type":"noul","score":1}}"#,
        r#"answer "q" has type "noul", want "score""#
    )]
    #[case::no_value(
        r#"{"q":{"type":"score","confidence":0.5}}"#,
        r#"answer "q" has no score value"#
    )]
    #[case::no_confidence(
        r#"{"q":{"type":"score","score":2}}"#,
        r#"answer "q" has no confidence"#
    )]
    fn should_reject_invalid_score_answer(
        #[case] answers: &str,
        #[case] want: &str,
    ) {
        let resp = parsed(answers);

        let err = resp.score("q").unwrap_err();

        assert_eq!(err.to_string(), format!("invalid response: {want}"));
    }

    #[rstest]
    #[case::missing(r#"{}"#, r#"answer "q" missing"#)]
    #[case::wrong_type(
        r#"{"q":{"type":""}}"#,
        r#"answer "q" has type "", want "choice""#
    )]
    #[case::no_choice(
        r#"{"q":{"type":"choice","confidence":1,"probabilities":{"a":1}}}"#,
        r#"answer "q" has no choice"#
    )]
    #[case::empty_choice(
        r#"{"q":{"type":"choice","choice":"","confidence":1,
            "probabilities":{"a":1}}}"#,
        r#"answer "q" has no choice"#
    )]
    #[case::no_confidence(
        r#"{"q":{"type":"choice","choice":"a","probabilities":{"a":1}}}"#,
        r#"answer "q" has no confidence"#
    )]
    #[case::no_probabilities(
        r#"{"q":{"type":"choice","choice":"a","confidence":1}}"#,
        r#"answer "q" has no probabilities"#
    )]
    #[case::absent_choice(
        r#"{"q":{"type":"choice","choice":"b","confidence":1,
            "probabilities":{"a":1}}}"#,
        r#"answer "q" chose "b", absent from probabilities"#
    )]
    fn should_reject_invalid_choice_answer(
        #[case] answers: &str,
        #[case] want: &str,
    ) {
        let resp = parsed(answers);

        let err = resp.choice("q").unwrap_err();

        assert_eq!(err.to_string(), format!("invalid response: {want}"));
    }

    /// Decodes a response whose `answers` member is `answers`.
    fn parsed(answers: &str) -> Response {
        decode(format!(r#"{{"answers":{answers}}}"#).as_bytes()).unwrap()
    }
}
