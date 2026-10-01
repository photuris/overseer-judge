//! Live tests against the real System One API.
//!
//! Every test is `#[ignore]`; run them with
//! `cargo test --test live -- --ignored`. A test returns early,
//! printing `skipped: no key`, when no API key is configured. The
//! criterion is the fixture labels and thresholds, never a comparison
//! of probabilities.

// Clippy exempts `#[test]` bodies but not their setup helpers, which
// panic on setup failure for the same reason the tests do.
#![expect(clippy::unwrap_used, reason = "test helpers")]

use std::{collections::BTreeMap, env, fs, path::Path, time::Duration};

use overseer_judge::{
    config::{self, Config},
    jev, review,
    session::{self, AgentKind},
    tasklint,
};
use serde::Deserialize;

/// Returns a client with `timeout`, or `None` when no key is
/// configured.
fn live_client(timeout: Duration) -> Option<jev::Client> {
    let env_key = env::var("TYPESAFE_API_KEY").ok();
    let Ok(key) = config::key(env_key.as_deref(), &config::default_key_file())
    else {
        println!("skipped: no key");

        return None;
    };
    let cfg = Config::from_env();

    Some(jev::Client::new(
        &cfg.base_url,
        &key,
        &cfg.model,
        Some(timeout),
    ))
}

/// Reads `name` from the fixture directory `dir`.
fn fixture(dir: &str, name: &str) -> String {
    fs::read_to_string(Path::new("tests/fixtures").join(dir).join(name))
        .unwrap()
}

/// Classifies every session fixture as [`AgentKind::Unknown`]: an
/// `ambiguous-*` fixture must stay below [`session::GATE`], every other
/// one must come back as its filename prefix. Runs every fixture
/// before failing.
#[test]
#[ignore = "calls the live API"]
fn should_label_session_fixtures() {
    let Some(client) = live_client(Duration::from_secs(30)) else {
        return;
    };
    let mut names: Vec<String> = fs::read_dir("tests/fixtures/session")
        .unwrap()
        .map(|entry| entry.unwrap().file_name().into_string().unwrap())
        .filter(|name| name.ends_with(".txt"))
        .collect();
    names.sort();
    assert!(!names.is_empty(), "no session fixtures");
    let mut failures = Vec::new();

    for name in &names {
        let raw = fixture("session", name);
        let verdict = match session::judge(&client, AgentKind::Unknown, &raw) {
            Ok(verdict) => verdict,
            Err(err) => {
                failures.push(format!("{name}: judge: {err}"));

                continue;
            }
        };
        println!(
            "{name}: state={} conf={:.2} coherent={:.2} hint={:?}",
            verdict.state,
            verdict.confidence,
            verdict.coherent,
            verdict.activity_hint
        );
        let want = name.split_once('-').map_or(name.as_str(), |(l, _)| l);

        if want == "ambiguous" {
            if verdict.confidence >= session::GATE {
                failures.push(format!(
                    "{name}: chose {:?} at {:.2}, want below {:.2} ({:?})",
                    verdict.state,
                    verdict.confidence,
                    session::GATE,
                    verdict.probabilities
                ));
            }

            continue;
        }

        if verdict.state != want {
            failures.push(format!(
                "{name}: state = {:?}, want {want:?} ({:?})",
                verdict.state, verdict.probabilities
            ));
        }
    }

    assert!(failures.is_empty(), "{failures:#?}");
}

/// Lints every task fixture in `expect.json`: `needs_interpretation`
/// and `scope_generic` thresholded at 0.5, `acceptance_sound` at
/// [`tasklint::SOUND_CUT`].
#[test]
#[ignore = "calls the live API"]
fn should_label_tasklint_fixtures() {
    let Some(client) = live_client(Duration::from_secs(60)) else {
        return;
    };
    let expect: BTreeMap<String, BTreeMap<String, bool>> =
        serde_json::from_str(&fixture("tasklint", "expect.json")).unwrap();
    assert!(!expect.is_empty(), "expect.json lists no fixtures");
    let mut failures = Vec::new();

    for (name, labels) in &expect {
        let path = format!("tests/fixtures/tasklint/{name}");
        let text = fixture("tasklint", name);
        let report = match tasklint::judge(&client, &path, &text) {
            Ok(report) => report,
            Err(err) => {
                failures.push(format!("{name}: judge: {err}"));

                continue;
            }
        };
        let (Some(judgments), Some(acceptance)) =
            (&report.judgments, &report.acceptance)
        else {
            failures.push(format!("{name}: no judgments"));

            continue;
        };
        println!(
            "{name}: judgments={judgments:?} acceptance={:.2} (conf {:.2})",
            acceptance.score, acceptance.confidence
        );

        for (id, &want) in labels {
            let got = if id == "acceptance_sound" {
                acceptance.score >= tasklint::SOUND_CUT
            } else {
                judgments.get(id).copied().unwrap_or_default() > 0.5
            };

            if got != want {
                failures.push(format!("{name}: {id} = {got}, want {want}"));
            }
        }
    }

    assert!(failures.is_empty(), "{failures:#?}");
}

/// One review item's labels in `expect.json`.
#[derive(Debug, Deserialize)]
struct ReviewLabels {
    /// Whether `style_only` should exceed 0.5.
    style_only: bool,
    /// The expected response kinds, in order.
    responses: Vec<String>,
}

/// Types every review item in `expect.json`: `style_only` thresholded
/// at 0.5, response kinds in order.
#[test]
#[ignore = "calls the live API"]
fn should_label_review_fixtures() {
    let Some(client) = live_client(Duration::from_secs(60)) else {
        return;
    };
    let expect: BTreeMap<String, BTreeMap<String, ReviewLabels>> =
        serde_json::from_str(&fixture("review", "expect.json")).unwrap();
    assert!(!expect.is_empty(), "expect.json lists no fixtures");
    let mut failures = Vec::new();

    for (file, items) in &expect {
        for item in review::parse(&fixture("review", file)) {
            let Some(want) = items.get(&item.id) else {
                failures
                    .push(format!("{file}: {} not in expect.json", item.id));

                continue;
            };
            let typed = match review::judge_item(&client, &item) {
                Ok(typed) => typed,
                Err(err) => {
                    failures.push(format!("{file} {}: judge: {err}", item.id));

                    continue;
                }
            };
            let kinds: Vec<&str> =
                typed.responses.iter().map(|r| r.kind.as_str()).collect();
            println!(
                "{file} {}: style_only={:.2} kinds={kinds:?}",
                typed.id, typed.style_only
            );

            if (typed.style_only > 0.5) != want.style_only {
                failures.push(format!(
                    "{file} {}: style_only = {:.2}, want {}",
                    typed.id, typed.style_only, want.style_only
                ));
            }

            if kinds != want.responses {
                failures.push(format!(
                    "{file} {}: kinds = {kinds:?}, want {:?}",
                    typed.id, want.responses
                ));
            }
        }
    }

    assert!(failures.is_empty(), "{failures:#?}");
}
