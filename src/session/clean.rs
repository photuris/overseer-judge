//! Cleans raw pane text and reads the input box and busy indicator out
//! of it.
//!
//! [`clean`] turns an ANSI pane capture into plain text the model can
//! judge. [`input_line`] and [`activity_hint`] then read what the code
//! can find on screen with certainty: the text in the agent's input
//! box and the agent's own busy-indicator line.

use std::sync::LazyLock;

use regex::Regex;

use super::AgentKind;

/// How many lines of a cleaned tail are kept.
pub const MAX_LINES: usize = 200;

/// How many bytes of a cleaned tail are kept.
pub const MAX_BYTES: usize = 12000;

/// The byte that opens every ANSI escape sequence.
const ESC: u8 = 0x1b;

/// The runes agents draw the left edge of their input box with.
const INPUT_MARKERS: [char; 2] = ['❯', '›'];

/// The rune opencode draws down the left edge of every composer line.
const BOX_BAR: char = '┃';

/// The rune opencode draws under the composer's last line.
const BOX_FOOT: char = '╹';

/// The rune opencode draws from the foot to the composer's right edge.
const BOX_CAP: char = '▀';

/// The rune pi draws its horizontal rules with.
const RULE: char = '─';

/// How many rule runes a line must end with to count as a rule.
const MIN_RULE: usize = 20;

/// How many other runes a labelled rule may carry.
const MAX_LABEL: usize = 24;

/// Matches one rune a pi rule label may hold: Go's `unicode.IsLetter`
/// (`L*`), `unicode.IsDigit` (`Nd`), or an ASCII space.
static LABEL_RUNE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[\p{L}\p{Nd} ]$")
        .unwrap_or_else(|_| unreachable!("the pattern is a valid literal"))
});

/// The prompts agents draw in an empty input box. Text starting with
/// one of them was not typed by a person.
// ponytail: literal prefixes; agents reword these across versions. Add
// entries here; the idle criterion's placeholder clause is the backstop.
const PLACEHOLDER_HINTS: [&str; 2] =
    ["Ask Codex to do anything", "Ask anything…"];

/// How many of a pane's last non-empty lines a busy indicator may hide
/// in.
const HINT_LINES: usize = 12;

/// How many runes of the busy-indicator line reach the model.
const MAX_HINT: usize = 100;

/// The phrases an agent draws beside its interrupt key while it works,
/// lower-cased. pi's static "escape interrupt" key hint is deliberately
/// not one of them: it is on screen whether or not the agent is busy.
const INTERRUPT_PHRASES: [&str; 2] = ["esc to interrupt", "esc interrupt"];

/// Finds a structure in the cleaned lines of a pane and returns the
/// text in it; `None` means the structure was absent, which differs
/// from it being present and empty.
type Strategy = fn(&[&str]) -> Option<String>;

// ── Cleaning ────────────────────────────────────────────────────────────────

/// Prepares a raw pane tail for judgment.
///
/// Steps, in order: drop every `\r`; remove faint spans (see
/// [`remove_faint`]); strip CSI, OSC, and two-byte escapes; drop
/// braille glyphs U+2800..U+28FF, which spinners are drawn with; trim
/// trailing whitespace from every line, collapse runs of three or more
/// blank lines to one, and drop trailing blank lines; keep the last
/// `max_lines` lines; and cap the text at `max_bytes` by dropping whole
/// lines from the front, then bytes from the front of what remains, cut
/// forward to a char boundary. A limit of 0 disables its step. The
/// result carries no trailing newline.
pub fn clean(raw: &str, max_lines: usize, max_bytes: usize) -> String {
    let text = raw.replace('\r', "");
    let text = strip_escapes(&remove_faint(text.as_bytes()));
    // Removing a two-byte escape can split a multi-byte char; the lossy
    // conversion turns the leftovers into U+FFFD, as Go's rune walk does.
    let text: String = String::from_utf8_lossy(&text)
        .chars()
        .filter(|c| !('\u{2800}'..='\u{28ff}').contains(c))
        .collect();
    let mut lines = collapse_blanks(&text);

    if max_lines > 0 && lines.len() > max_lines {
        lines.drain(..lines.len() - max_lines);
    }

    cap_bytes(&lines, max_bytes)
}

/// The faint transition an SGR parameter list leaves behind.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Faint {
    /// Faint is turned off.
    Off,
    /// Faint is left as it was.
    Unchanged,
    /// Faint is turned on.
    On,
}

/// A complete CSI sequence found in a byte string.
struct Csi<'a> {
    /// Length of the whole sequence in bytes, `ESC [` included.
    len: usize,
    /// The final byte, which names the operation (`m` for SGR).
    final_byte: u8,
    /// The bytes between `ESC [` and the final byte.
    params: &'a [u8],
}

/// Deletes every faint-rendered span, escapes included.
///
/// A span starts at an SGR sequence that turns faint on and ends just
/// past the next one that turns it off, or at the end of its line.
/// Claude Code renders its greyed-out prompt suggestion this way; it is
/// not typed input and must not reach the model.
fn remove_faint(s: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(s.len());
    let mut i = 0;

    while i < s.len() {
        match csi_at(s, i) {
            Some(csi)
                if csi.final_byte == b'm'
                    && sgr_faint(csi.params) == Faint::On =>
            {
                i = skip_faint(s, i + csi.len);
            }
            _ => {
                out.push(s[i]);
                i += 1;
            }
        }
    }

    out
}

/// Returns the offset just past the faint span that continues at `i`:
/// past the SGR sequence that turns faint off, or at the newline that
/// ends the line, or the end of `s`.
fn skip_faint(s: &[u8], mut i: usize) -> usize {
    while i < s.len() {
        if s[i] == b'\n' {
            return i;
        }

        match csi_at(s, i) {
            None => i += 1,
            Some(csi)
                if csi.final_byte == b'm'
                    && sgr_faint(csi.params) == Faint::Off =>
            {
                return i + csi.len;
            }
            Some(csi) => i += csi.len,
        }
    }

    i
}

/// Walks the `;`-separated parameters of one SGR sequence, left to
/// right, and returns the faint transition it leaves behind.
///
/// An extended-colour selector (38, 48, 58) consumes its own arguments,
/// so the 2 in `38;5;2` or `38;2;r;g;b` is a colour value and never
/// turns faint on, and the 22 in `48;5;22` never turns it off. A
/// parameter with colon sub-parameters is one whole colour operation. A
/// standalone 2 turns faint on; a standalone 0, a 22, or an empty
/// parameter (including the empty list of a bare `CSI m`) turns it off.
fn sgr_faint(params: &[u8]) -> Faint {
    if params.is_empty() {
        return Faint::Off;
    }

    let mut fields = params.split(|&b| b == b';');
    let mut state = Faint::Unchanged;

    while let Some(field) = fields.next() {
        if field.contains(&b':') {
            continue;
        }

        if matches!(field, b"38" | b"48" | b"58") {
            // A list that ends before its arguments consumes what is
            // left; `take` stops there on its own.
            let args = match fields.clone().next() {
                Some(b"5") => 2,
                Some(b"2") => 4,
                _ => 0,
            };
            fields.by_ref().take(args).for_each(drop);

            continue;
        }

        match field {
            b"2" => state = Faint::On,
            b"0" | b"22" | b"" => state = Faint::Off,
            _ => {}
        }
    }

    state
}

/// Returns the complete CSI sequence starting at `s[i]`, if there is
/// one.
fn csi_at(s: &[u8], i: usize) -> Option<Csi<'_>> {
    if s.get(i..i + 2)? != [ESC, b'['] {
        return None;
    }

    let end =
        i + 2 + s[i + 2..].iter().position(|b| (0x40..=0x7e).contains(b))?;

    Some(Csi {
        len: end - i + 1,
        final_byte: s[end],
        params: &s[i + 2..end],
    })
}

/// Removes every ANSI escape sequence.
///
/// A sequence left unterminated by a truncated tail takes the rest of
/// the text with it, so half an escape never reaches the model.
fn strip_escapes(s: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(s.len());
    let mut i = 0;

    while i < s.len() {
        if s[i] != ESC {
            out.push(s[i]);
            i += 1;

            continue;
        }

        let Some(len) = escape_len(s, i) else {
            break;
        };

        i += len;
    }

    out
}

/// Returns the length of the escape sequence at `s[i]`, or `None` when
/// it is unterminated.
fn escape_len(s: &[u8], i: usize) -> Option<usize> {
    match s.get(i + 1)? {
        b'[' => csi_at(s, i).map(|csi| csi.len),
        b']' => osc_len(s, i),
        _ => Some(2),
    }
}

/// Returns the length of the OSC sequence at `s[i]`, which ends at a
/// BEL or at a string terminator (`ESC \`).
fn osc_len(s: &[u8], i: usize) -> Option<usize> {
    (i + 2..s.len()).find_map(|j| {
        if s[j] == 0x07 {
            Some(j - i + 1)
        } else if s[j] == ESC && s.get(j + 1) == Some(&b'\\') {
            Some(j - i + 2)
        } else {
            None
        }
    })
}

/// Splits `text` into lines with trailing whitespace trimmed, any run
/// of three or more blank lines collapsed to one, and trailing blank
/// lines dropped.
fn collapse_blanks(text: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut blanks = 0;

    for line in text.split('\n') {
        let line = line.trim_end();

        if line.is_empty() {
            blanks += 1;

            continue;
        }

        let run = if blanks >= 3 { 1 } else { blanks };

        out.extend(std::iter::repeat_n("", run));
        out.push(line);
        blanks = 0;
    }

    out
}

/// Joins `lines`, dropping whole lines from the front, and then bytes
/// from the front of what remains, until the result fits in
/// `max_bytes`. A `max_bytes` of 0 disables the cap.
fn cap_bytes(lines: &[&str], max_bytes: usize) -> String {
    if max_bytes == 0 {
        return lines.join("\n");
    }

    let mut lines = lines;
    let mut total = lines
        .iter()
        .map(|line| line.len() + 1)
        .sum::<usize>()
        .saturating_sub(1);

    while lines.len() > 1 && total > max_bytes {
        total -= lines[0].len() + 1;
        lines = &lines[1..];
    }

    let text = lines.join("\n");

    if text.len() <= max_bytes {
        return text;
    }

    let start = (text.len() - max_bytes..text.len())
        .find(|&i| text.is_char_boundary(i))
        .unwrap_or(text.len());

    text[start..].to_owned()
}

// ── Input line ──────────────────────────────────────────────────────────────

/// Returns the text sitting in the agent's input box, or `""`.
///
/// `cleaned` is [`clean`] output. The strategies for `kind` run in
/// order and the FIRST one that finds its structure wins, even when the
/// text it finds is empty; a known placeholder hint is then dropped.
///
/// - claude, codex: the marker line
/// - opencode: the composer box
/// - pi: the text between two rules
/// - unknown: marker, then box, then rules
pub fn input_line(kind: AgentKind, cleaned: &str) -> String {
    let lines: Vec<&str> = cleaned.split('\n').collect();
    let strategies: &[Strategy] = match kind {
        AgentKind::Claude | AgentKind::Codex => &[marker_input],
        AgentKind::Opencode => &[box_input],
        AgentKind::Pi => &[rules_input],
        AgentKind::Unknown => &[marker_input, box_input, rules_input],
    };

    strategies
        .iter()
        .find_map(|find| find(&lines))
        .map(drop_placeholder)
        .unwrap_or_default()
}

/// Returns `""` when `text` starts with a placeholder hint, and `text`
/// unchanged otherwise.
fn drop_placeholder(text: String) -> String {
    if PLACEHOLDER_HINTS.iter().any(|hint| text.starts_with(hint)) {
        return String::new();
    }

    text
}

/// Finds the last line whose first rune is an input marker and returns
/// it with the marker and any following spaces or U+00A0 removed, then
/// trimmed. A menu option under a dialog cursor is not typed input and
/// yields `""`, as does a bare marker.
fn marker_input(lines: &[&str]) -> Option<String> {
    lines.iter().rev().find_map(|line| {
        let text = line
            .strip_prefix(INPUT_MARKERS)?
            .trim_start_matches([' ', '\u{a0}'])
            .trim();

        if is_menu_option(text) {
            return Some(String::new());
        }

        Some(text.to_owned())
    })
}

/// Reports whether `text` opens like a dialog menu option, such as
/// `1. Yes, continue`: ASCII digits, a dot, and an ASCII space, tab,
/// newline, form feed, or carriage return.
fn is_menu_option(text: &str) -> bool {
    let rest = text.trim_start_matches(|c: char| c.is_ascii_digit());

    rest.len() < text.len()
        && rest
            .strip_prefix('.')
            .and_then(|after| after.chars().next())
            .is_some_and(|c| matches!(c, ' ' | '\t' | '\n' | '\x0c' | '\r'))
}

/// Finds opencode's composer: the lowest run of lines whose first
/// non-space rune is a bar, directly above a line whose first non-space
/// rune is the foot.
fn box_input(lines: &[&str]) -> Option<String> {
    for (foot, closer) in lines.iter().enumerate().rev() {
        if first_rune(closer) != Some(BOX_FOOT) {
            continue;
        }

        let top = lines[..foot]
            .iter()
            .rposition(|line| first_rune(line) != Some(BOX_BAR))
            .map_or(0, |above| above + 1);

        if top == foot {
            continue;
        }

        let (lo, hi) = box_edges(closer);

        return Some(box_text(&lines[top..foot], lo, hi));
    }

    None
}

/// Returns the char index of the (last) foot in a closer line and of
/// the last cap rune, which are the box's left and right edges.
/// Anything further right on a box line belongs to whatever the agent
/// drew beside it. A closer with no cap runes clips to its own last
/// rune.
fn box_edges(closer: &str) -> (usize, usize) {
    let mut lo = 0;
    let mut hi = closer.chars().count().saturating_sub(1);

    for (i, c) in closer.chars().enumerate() {
        match c {
            BOX_FOOT => lo = i,
            BOX_CAP => hi = i,
            _ => {}
        }
    }

    (lo, hi)
}

/// Takes the runes strictly right of `lo` and up to `hi` out of every
/// composer line, drops the blank ones, drops the last remaining one
/// (opencode's status line), and joins the rest with single spaces.
// ponytail: char indices, not display columns; double-width input would
// shift the clip. Switch to a width-aware walk if that bites.
fn box_text(box_lines: &[&str], lo: usize, hi: usize) -> String {
    let content: Vec<String> = box_lines
        .iter()
        .filter_map(|line| {
            let inside: String =
                line.chars().take(hi + 1).skip(lo + 1).collect();
            let inside = inside.trim();

            (!inside.is_empty()).then(|| inside.to_owned())
        })
        .collect();

    content
        .split_last()
        .map(|(_status, input)| input.join(" "))
        .unwrap_or_default()
}

/// Finds pi's composer: the non-empty lines between the last two rules,
/// trimmed and joined with single spaces. `None` unless two rules
/// exist.
fn rules_input(lines: &[&str]) -> Option<String> {
    let mut rules = lines
        .iter()
        .enumerate()
        .rev()
        .filter(|(_, line)| is_rule(line))
        .map(|(i, _)| i);
    let last = rules.next()?;
    let prev = rules.next()?;

    let content: Vec<&str> = lines[prev + 1..last]
        .iter()
        .map(|line| line.trim())
        .filter(|line| !line.is_empty())
        .collect();

    Some(content.join(" "))
}

/// Reports whether a line is one of pi's horizontal rules.
///
/// The trimmed line opens with two rule runes and closes with at least
/// [`MIN_RULE`] of them. Between the runs pi may draw a short label, as
/// in `── Working ───…`, so up to [`MAX_LABEL`] other runes are allowed,
/// all [`LABEL_RUNE`] matches.
fn is_rule(line: &str) -> bool {
    let line = line.trim();
    let tail = line.chars().rev().take_while(|&c| c == RULE).count();
    let mut label = line.chars().filter(|&c| c != RULE);

    line.starts_with("──")
        && tail >= MIN_RULE
        && label
            .by_ref()
            .take(MAX_LABEL)
            .all(|c| LABEL_RUNE.is_match(c.encode_utf8(&mut [0; 4])))
        && label.next().is_none()
}

/// Returns a line's first non-whitespace rune, if any.
fn first_rune(line: &str) -> Option<char> {
    line.chars().find(|c| !c.is_whitespace())
}

// ── Activity hint ───────────────────────────────────────────────────────────

/// Returns the agent's own busy-indicator line in `cleaned` ([`clean`]
/// output), or `""`.
///
/// Only the last [`HINT_LINES`] non-empty lines are searched. The
/// strategies for `kind` run in order and the FIRST one that finds its
/// shape wins:
///
/// - claude, codex, opencode: the interrupt-phrase line
/// - pi: the label on a rule
/// - unknown: interrupt phrase, then rule label
// ponytail: a pane that merely quotes "esc to interrupt" in its last 12
// lines reads as busy. Tighten to per-agent line shapes if that bites.
pub fn activity_hint(kind: AgentKind, cleaned: &str) -> String {
    let mut lines: Vec<&str> = cleaned
        .split('\n')
        .rev()
        .filter(|line| !line.trim().is_empty())
        .take(HINT_LINES)
        .collect();
    lines.reverse();

    let strategies: &[Strategy] = match kind {
        AgentKind::Claude | AgentKind::Codex | AgentKind::Opencode => {
            &[interrupt_hint]
        }
        AgentKind::Pi => &[label_hint],
        AgentKind::Unknown => &[interrupt_hint, label_hint],
    };

    strategies
        .iter()
        .find_map(|find| find(&lines))
        .unwrap_or_default()
}

/// Returns the last line carrying an interrupt phrase, its whitespace
/// collapsed and cut to [`MAX_HINT`] runes. Matching folds ASCII A-Z
/// only, so a rune such as U+0130 never folds into a phrase.
fn interrupt_hint(lines: &[&str]) -> Option<String> {
    let line = lines.iter().rev().find(|line| {
        let lower = line.to_ascii_lowercase();

        INTERRUPT_PHRASES
            .iter()
            .any(|phrase| lower.contains(phrase))
    })?;

    Some(collapse_spaces(line).chars().take(MAX_HINT).collect())
}

/// Returns the label on the last labelled rule, which is how pi says it
/// is working: `──  Working ────…` yields `Working`. A rule of nothing
/// but rule runes carries no label and is skipped; one labelled only
/// with spaces is found and empty. The label comes off the trimmed
/// line, so an indented plain rule stays unlabelled.
fn label_hint(lines: &[&str]) -> Option<String> {
    lines
        .iter()
        .rev()
        .filter(|line| is_rule(line))
        .find_map(|line| {
            let label: String =
                line.trim().chars().filter(|&c| c != RULE).collect();

            (!label.is_empty()).then(|| collapse_spaces(&label))
        })
}

/// Trims a line and collapses every run of whitespace in it to one
/// space.
fn collapse_spaces(line: &str) -> String {
    line.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A pi rule exactly [`MIN_RULE`] runes long.
    fn rule() -> String {
        RULE.to_string().repeat(MIN_RULE)
    }

    /// A pi rule one rune too short to count.
    fn short_rule() -> String {
        RULE.to_string().repeat(MIN_RULE - 1)
    }

    /// One of pi's labelled rules, as in `── Working ──…`.
    fn labelled(label: &str) -> String {
        format!("── {label} {}", rule())
    }

    /// How many runes of a [`boxed_at`] line fall inside a box whose cap
    /// is `BOX_INNER + 2` runes wide.
    const BOX_INNER: usize = 30;

    /// An opencode box whose cap reaches past its longest line.
    fn composer(lines: &[&str]) -> String {
        let width = lines.iter().map(|l| l.chars().count()).max();

        boxed_at(width.unwrap_or(0) + 2, lines)
    }

    /// An opencode box whose cap stops `cap_width` runes past the foot.
    fn boxed_at(cap_width: usize, lines: &[&str]) -> String {
        let mut out: Vec<String> =
            lines.iter().map(|line| format!("  ┃  {line}")).collect();
        out.push(format!("  ╹{}", "▀".repeat(cap_width)));

        out.join("\n")
    }

    /// Pads `text` to the box's inner width and draws `side` past its
    /// right edge, as opencode draws its file sidebar.
    fn beside(text: &str, side: &str) -> String {
        let pad = BOX_INNER - text.chars().count();

        format!("{text}{}{side}", " ".repeat(pad))
    }

    /// `n` lines of ordinary output.
    fn filler(n: usize) -> Vec<String> {
        vec!["some output".to_owned(); n]
    }

    /// `line` above `n` non-empty lines.
    fn above(line: &str, n: usize) -> String {
        let mut lines = vec![line.to_owned()];
        lines.extend(filler(n));

        lines.join("\n")
    }

    /// Runs [`clean`] with limits off on every `(name, raw, want)` case
    /// and panics listing every mismatch.
    fn assert_cleans(cases: &[(&str, &str, &str)]) {
        let failures: Vec<String> = cases
            .iter()
            .filter_map(|&(name, raw, want)| {
                let got = clean(raw, 0, 0);

                (got != want).then(|| {
                    format!("{name}: clean({raw:?}) = {got:?}, want {want:?}")
                })
            })
            .collect();

        assert!(failures.is_empty(), "{}", failures.join("\n"));
    }

    #[test]
    fn should_remove_faint_spans() {
        assert_cleans(&[
            ("whole line", "\x1b[2mghost\x1b[0m", ""),
            ("reset then faint", "\x1b[0;2mghost\x1b[22mkept", "kept"),
            ("text before", "typed \x1b[2mghost\x1b[0m", "typed"),
            (
                "unterminated ends at the line",
                "a\x1b[2mghost\nkept",
                "a\nkept",
            ),
        ]);
    }

    #[test]
    fn should_read_colour_arguments_as_colours_in_faint_parsing() {
        assert_cleans(&[
            ("indexed colour 2", "\x1b[38;5;2mvisible\x1b[0m", "visible"),
            (
                "rgb colour with a 2 channel",
                "\x1b[38;2;100;150;200mvisible\x1b[0m",
                "visible",
            ),
            (
                "indexed background colour 22",
                "\x1b[48;5;22mvisible\x1b[0m",
                "visible",
            ),
            (
                "colon sub-parameters",
                "\x1b[38:2::10:20:30mvisible\x1b[0m",
                "visible",
            ),
            (
                "indexed colour 22 does not end a faint span",
                "\x1b[2mghost\x1b[38;5;22m still ghost\x1b[0mkept",
                "kept",
            ),
            (
                "a zero rgb channel does not end a faint span",
                "\x1b[2mghost\x1b[38;2;0;120;130m still\x1b[0mkept",
                "kept",
            ),
            (
                "reset then faint stays on",
                "\x1b[0;2mghost\x1b[22mkept",
                "kept",
            ),
            ("faint then reset never starts", "\x1b[2;22mkept", "kept"),
        ]);
    }

    #[test]
    fn should_handle_lines_blanks_and_limits() {
        let cases = [
            (
                "collapses a long blank run",
                "a\n\n\n\n\n\nb",
                0,
                0,
                "a\n\nb",
            ),
            ("keeps a short blank run", "a\n\nb", 0, 0, "a\n\nb"),
            ("trims trailing whitespace", "a   \nb\t", 0, 0, "a\nb"),
            ("drops the trailing newline", "a\nb\n\n", 0, 0, "a\nb"),
            ("honours max_lines", "1\n2\n3\n4", 2, 0, "3\n4"),
            (
                "drops whole lines for max_bytes",
                "aaa\nbbb\nccc",
                0,
                8,
                "bbb\nccc",
            ),
            (
                "cuts an oversize line at a char boundary",
                "ααααα",
                0,
                5,
                "αα",
            ),
            ("zero limits disable", "1\n2\n3\n4", 0, 0, "1\n2\n3\n4"),
        ];

        let failures: Vec<String> = cases
            .iter()
            .filter_map(|&(name, raw, max_lines, max_bytes, want)| {
                let got = clean(raw, max_lines, max_bytes);

                (got != want)
                    .then(|| format!("{name}: got {got:?}, want {want:?}"))
            })
            .collect();

        assert!(failures.is_empty(), "{}", failures.join("\n"));
    }

    #[test]
    fn should_strip_escapes_and_glyphs() {
        assert_cleans(&[
            ("csi colour", "\x1b[31mred\x1b[39m text", "red text"),
            ("osc title", "\x1b]0;a title\x07hello", "hello"),
            ("osc string terminator", "\x1b]0;a title\x1b\\hi", "hi"),
            ("two-byte escape", "a\x1bMb", "ab"),
            ("braille spinner", "load⠋⠙ing", "loading"),
            ("carriage returns", "a\r\nb\r", "a\nb"),
            ("no escapes", "plain text", "plain text"),
            ("unterminated escape", "kept\x1b[31", "kept"),
        ]);
    }

    #[test]
    fn should_extract_input_line() {
        use AgentKind::{Claude, Codex, Opencode, Pi, Unknown};

        let rule = rule();
        let cases: Vec<(&str, AgentKind, String, &str)> = vec![
            // marker
            ("claude composer", Claude, "❯ go ahead".into(), "go ahead"),
            ("bare marker", Claude, "❯\u{a0}".into(), ""),
            ("codex hint", Codex, "› Ask Codex to do anything".into(), ""),
            ("menu option", Codex, "› 1. Yes, continue".into(), ""),
            (
                "the last marker line wins",
                Claude,
                "❯ first\nsome output\n❯ second".into(),
                "second",
            ),
            (
                "a boxed marker is not the input line",
                Claude,
                "│ ❯ 1. Yes, proceed │".into(),
                "",
            ),
            ("no marker", Claude, "just output\nmore output".into(), ""),
            ("empty", Claude, String::new(), ""),
            (
                "a ghost suggestion is not input",
                Claude,
                clean("❯ \x1b[2mghost\x1b[38;5;22m suggestion\x1b[0m", 0, 0),
                "",
            ),
            // box
            (
                "the box joins its input lines",
                Opencode,
                composer(&["first line", "second line", "status"]),
                "first line second line",
            ),
            (
                "a box holding only a status line is empty",
                Opencode,
                composer(&["", "status", ""]),
                "",
            ),
            (
                "the bottom box wins",
                Opencode,
                format!(
                    "{}\nreply\n{}",
                    composer(&["upper", "status"]),
                    composer(&["lower", "status"])
                ),
                "lower",
            ),
            (
                "a bar run without a foot is not a box",
                Opencode,
                "  ┃  quoted reply\n  ┃  more reply".into(),
                "",
            ),
            (
                "a sidebar past the cap is not input",
                Opencode,
                boxed_at(
                    BOX_INNER + 2,
                    &[
                        &beside("typed here", "/tmp/claude-1000/-home-"),
                        &beside("", "joshua-Projects-agent-skills/"),
                        &beside("Build · medium", "23edc16b-74db-4b4f"),
                    ],
                ),
                "typed here",
            ),
            (
                "a box line shorter than the cap keeps what it has",
                Opencode,
                boxed_at(60, &["short", "status"]),
                "short",
            ),
            (
                "a closer with no cap clips to its own length",
                Opencode,
                format!(
                    "  ┃  typed here is long\n  ┃  status\n  ╹{}",
                    "▔".repeat(10)
                ),
                "typed he",
            ),
            // rules
            (
                "the text between the last two rules",
                Pi,
                format!("{rule}\nnotice\n{rule}\ntyped\n{rule}"),
                "typed",
            ),
            (
                "an empty composer between two rules",
                Pi,
                format!("{rule}\ntyped\n{rule}\n\n{rule}"),
                "",
            ),
            (
                "one rule is not a composer",
                Pi,
                format!("typed\n{rule}"),
                "",
            ),
            (
                "a short rule is not a rule",
                Pi,
                format!("{}\ntyped\n{rule}", short_rule()),
                "",
            ),
            (
                "rules survive CRLF through clean",
                Pi,
                clean(&format!("{rule}\r\ntyped\r\n{rule}"), 0, 0),
                "typed",
            ),
            (
                "a labelled rule is a rule",
                Pi,
                format!("{}\ntyped\n{rule}", labelled(" Working")),
                "typed",
            ),
            (
                "an empty composer under a labelled rule",
                Pi,
                format!(
                    "{rule}\nstreamed output\n{}\n{rule}",
                    labelled(" Working")
                ),
                "",
            ),
            // strategy selection
            (
                "pi ignores a marker line",
                Pi,
                format!("{rule}\ntyped\n{rule}\n❯ ignored"),
                "typed",
            ),
            (
                "claude ignores rules",
                Claude,
                format!("{rule}\ntyped\n{rule}"),
                "",
            ),
            (
                "unknown prefers the marker over rules",
                Unknown,
                format!("{rule}\ntyped\n{rule}\n❯ marked"),
                "marked",
            ),
            (
                "unknown keeps an empty marker over rules",
                Unknown,
                format!("{rule}\ntyped\n{rule}\n❯"),
                "",
            ),
            (
                "unknown prefers the box over rules",
                Unknown,
                format!(
                    "{rule}\ntyped\n{rule}\n{}",
                    composer(&["", "status"])
                ),
                "",
            ),
            (
                "unknown falls through a bar run to the rules",
                Unknown,
                format!("{rule}\ntyped\n{rule}\n  ┃  reply"),
                "typed",
            ),
            // placeholders
            (
                "a bare placeholder is dropped",
                Opencode,
                composer(&["Ask anything…", "status"]),
                "",
            ),
            (
                "a placeholder with an example is dropped",
                Opencode,
                composer(&[
                    r#"Ask anything… "What is the tech stack?""#,
                    "status",
                ]),
                "",
            ),
            (
                "a typed question that starts alike is kept",
                Opencode,
                composer(&["Ask anything about X", "status"]),
                "Ask anything about X",
            ),
        ];

        let failures: Vec<String> = cases
            .iter()
            .filter_map(|(name, kind, cleaned, want)| {
                let got = input_line(*kind, cleaned);

                (got != *want).then(|| {
                    format!(
                        "{name}: input_line({kind:?}) = {got:?}, want {want:?}"
                    )
                })
            })
            .collect();

        assert!(failures.is_empty(), "{}", failures.join("\n"));
    }

    #[test]
    fn should_recognise_pi_rules() {
        let cases = [
            ("a plain rule", rule(), true),
            ("a labelled rule", labelled(" Working"), true),
            (
                "a label that is too long",
                labelled("a label that is far too long to be a rule label"),
                false,
            ),
            ("a label with punctuation", labelled(" Work/ing"), false),
            (
                "a short tail",
                format!("──  Working {}", short_rule()),
                false,
            ),
            ("a short rule", short_rule(), false),
            ("not a rule at all", "typed text".to_owned(), false),
        ];

        let failures: Vec<String> = cases
            .iter()
            .filter(|(_, line, want)| is_rule(line) != *want)
            .map(|(name, line, want)| {
                format!("{name}: is_rule({line:?}) != {want}")
            })
            .collect();

        assert!(failures.is_empty(), "{}", failures.join("\n"));
    }

    #[test]
    fn should_extract_activity_hint() {
        use AgentKind::{Claude, Opencode, Pi, Unknown};

        const BUSY: &str = "✶ Wrangling… (esc to interrupt · 1m 14s)";
        let rule = rule();
        let long = format!("esc to interrupt {}", "x".repeat(103));
        let cut = format!("esc to interrupt {}", "x".repeat(83));
        let cases: Vec<(&str, AgentKind, String, &str)> = vec![
            ("nothing on screen", Claude, "all done\n❯".into(), ""),
            ("empty", Unknown, String::new(), ""),
            ("the claude footer", Claude, BUSY.into(), BUSY),
            (
                "case does not matter",
                Claude,
                "ESC TO INTERRUPT".into(),
                "ESC TO INTERRUPT",
            ),
            (
                "uppercase matches without the to as well",
                Opencode,
                "ESC INTERRUPT".into(),
                "ESC INTERRUPT",
            ),
            (
                "a dotted capital I does not fold to ASCII i",
                Opencode,
                "ESC \u{130}NTERRUPT".into(),
                "",
            ),
            (
                "opencode drops the to",
                Opencode,
                "⬝⬝⬝⬝  esc interrupt  tab agents".into(),
                "⬝⬝⬝⬝ esc interrupt tab agents",
            ),
            (
                "pi's static key hint is not a busy indicator",
                Unknown,
                "escape interrupt · ctrl+c quit".into(),
                "",
            ),
            (
                "the twelfth-from-last line still counts",
                Claude,
                above(BUSY, 11),
                BUSY,
            ),
            (
                "the thirteenth-from-last line does not",
                Claude,
                above(BUSY, 12),
                "",
            ),
            (
                "blank lines do not fill the window",
                Claude,
                format!("{BUSY}\n\n\n{}", filler(11).join("\n")),
                BUSY,
            ),
            (
                "the last matching line wins",
                Claude,
                "first (esc to interrupt)\nlast (esc to interrupt)".into(),
                "last (esc to interrupt)",
            ),
            ("a long line is cut to 100 runes", Claude, long, &cut),
            // labelled rules
            (
                "pi reads its labelled rule",
                Pi,
                format!("streamed output\n{}", labelled(" Working")),
                "Working",
            ),
            (
                "a plain rule carries no label",
                Pi,
                format!("all done\n{rule}"),
                "",
            ),
            (
                "a rule labelled with spaces is found and empty",
                Pi,
                format!("all done\n── {rule}"),
                "",
            ),
            (
                "an indented plain rule hides no earlier label",
                Pi,
                format!("{}\n  {rule}", labelled(" Working")),
                "Working",
            ),
            (
                "an indented plain rule hides none for unknown",
                Unknown,
                format!("{}\n  {rule}", labelled(" Working")),
                "Working",
            ),
            (
                "the last labelled rule wins",
                Pi,
                format!("{}\n{}", labelled(" Thinking"), labelled(" Working")),
                "Working",
            ),
            // strategy selection
            ("pi ignores an interrupt line", Pi, BUSY.into(), ""),
            (
                "claude ignores a labelled rule",
                Claude,
                labelled(" Working"),
                "",
            ),
            (
                "opencode ignores a labelled rule",
                Opencode,
                labelled(" Working"),
                "",
            ),
            (
                "unknown reads a labelled rule",
                Unknown,
                labelled(" Working"),
                "Working",
            ),
            (
                "unknown prefers the interrupt line",
                Unknown,
                format!("{}\n{BUSY}", labelled(" Working")),
                BUSY,
            ),
        ];

        let failures: Vec<String> = cases
            .iter()
            .filter_map(|(name, kind, cleaned, want)| {
                let got = activity_hint(*kind, cleaned);

                (got != *want).then(|| {
                    format!("{name}: activity_hint({kind:?}) = {got:?}, want {want:?}")
                })
            })
            .collect();

        assert!(failures.is_empty(), "{}", failures.join("\n"));
    }
}
