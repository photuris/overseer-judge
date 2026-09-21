package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"overseer-judge/internal/session"
)

// fixedResponse is the body the success-golden server returns.
const fixedResponse = `{"model":"jev-latest","answers":` +
	`{"urgent":{"type":"noul","noul":0.82}},` +
	`"usage":{"input_tokens":21,"output_tokens":2}}`

// validRequest is a minimal well-formed raw request.
const validRequest = `{"state":"hi","questions":` +
	`{"q":{"type":"noul","instructions":"x"}}}`

// result captures one Run invocation.
type result struct {
	code           int
	stdout, stderr string
}

// invoke runs Run with stdin and returns its code and streams.
func invoke(t *testing.T, stdin string, args ...string) result {
	t.Helper()

	var out, errOut bytes.Buffer
	code := Run(
		context.Background(), args,
		strings.NewReader(stdin), &out, &errOut,
	)

	return result{code, out.String(), errOut.String()}
}

// noKey points keyFile at a path that does not exist and clears the
// key environment variable.
func noKey(t *testing.T) {
	t.Helper()

	t.Setenv("TYPESAFE_API_KEY", "")
	prev := keyFile
	keyFile = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { keyFile = prev })
}

// withKey points keyFile at a file holding key.
func withKey(t *testing.T, key string) {
	t.Helper()

	t.Setenv("TYPESAFE_API_KEY", "")
	path := filepath.Join(t.TempDir(), "jev")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	prev := keyFile
	keyFile = path
	t.Cleanup(func() { keyFile = prev })
}

// errType returns the "type" of the JSON error record on the last
// stderr line.
func errType(t *testing.T, stderr string) string {
	t.Helper()

	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	last := lines[len(lines)-1]

	var rec errorRecord
	if err := json.Unmarshal([]byte(last), &rec); err != nil {
		t.Fatalf("last stderr line %q is not an error record: %v",
			last, err)
	}

	return rec.Error.Type
}

// ── Help, version, dispatch ─────────────────────────────────────────────────

func TestHelpSnapshots(t *testing.T) {
	tests := []struct {
		name string
		args []string
		file string
	}{
		{"top level", []string{"--help"}, "help.txt"},
		{"verb", []string{"raw", "--help"}, "help-raw.txt"},
		{"session verb", []string{"session", "--help"},
			"help-session.txt"},
		{"task verb", []string{"task", "--help"}, "help-task.txt"},
		{"review verb", []string{"review", "--help"},
			"help-review.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := os.ReadFile(
				filepath.Join("testdata", tt.file),
			)
			if err != nil {
				t.Fatalf("read snapshot: %v", err)
			}

			got := invoke(t, "", tt.args...)
			if got.code != 0 {
				t.Errorf("code = %d, want 0", got.code)
			}
			if got.stdout != string(want) {
				t.Errorf("stdout does not match %s:\n%s",
					tt.file, got.stdout)
			}
			if got.stderr != "" {
				t.Errorf("stderr = %q, want empty", got.stderr)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	got := invoke(t, "", "--version")
	if got.code != 0 || got.stdout != "dev\n" {
		t.Errorf("Run(--version) = %d, %q", got.code, got.stdout)
	}
}

func TestNoArgsAndUnknownVerb(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}} {
		got := invoke(t, "", args...)
		if got.code != exitUsage {
			t.Errorf("code = %d, want %d", got.code, exitUsage)
		}
		if got.stdout != "" {
			t.Errorf("stdout = %q, want empty", got.stdout)
		}
		if !strings.Contains(got.stderr, "overseer-judge <verb>") {
			t.Errorf("stderr lacks the usage line: %q", got.stderr)
		}
		if ty := errType(t, got.stderr); ty != "usage" {
			t.Errorf("error type = %q, want usage", ty)
		}
	}
}

// ── raw --dry-run ───────────────────────────────────────────────────────────

// dryRunBody unmarshals a dry-run record from stdout.
func dryRunBody(t *testing.T, stdout string) map[string]any {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", stdout, err)
	}

	return got
}

func TestRawDryRun(t *testing.T) {
	noKey(t)
	t.Setenv("TYPESAFE_DEFAULT_MODEL", "jev-test")

	got := invoke(t, validRequest, "raw", "--dry-run", "--input", "-")
	if got.code != 0 {
		t.Fatalf("code = %d, stderr = %s", got.code, got.stderr)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want empty", got.stderr)
	}

	const want = `{"method":"POST","path":"/v1/systemone","body":` +
		`{"model":"jev-test","questions":{"q":{"instructions":"x",` +
		`"type":"noul"}},"state":"hi"}}`
	if !reflect.DeepEqual(
		dryRunBody(t, got.stdout), dryRunBody(t, want),
	) {
		t.Errorf("stdout = %s\nwant equivalent to %s", got.stdout, want)
	}
	if !strings.HasSuffix(got.stdout, "}\n") {
		t.Errorf("stdout %q does not end in a newline", got.stdout)
	}
}

func TestRawDryRunModelFlag(t *testing.T) {
	noKey(t)
	t.Setenv("TYPESAFE_DEFAULT_MODEL", "jev-test")

	got := invoke(t, validRequest,
		"raw", "--dry-run", "--model", "m2", "--input", "-")
	if got.code != 0 {
		t.Fatalf("code = %d, stderr = %s", got.code, got.stderr)
	}

	body := dryRunBody(t, got.stdout)["body"].(map[string]any)
	if body["model"] != "m2" {
		t.Errorf("body.model = %v, want m2", body["model"])
	}
}

func TestRawUsageErrors(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"null body", "null",
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"trailing value", "{} trailing",
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"trailing bracket", validRequest + "]",
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"trailing brace", validRequest + "}",
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"trailing bracket and object", validRequest + "] {}",
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"two objects", validRequest + " " + validRequest,
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"missing questions", `{"state":"hi"}`,
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"missing state", `{"questions":{}}`,
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"input absent", validRequest,
			[]string{"raw", "--dry-run"}},
		{"not an object", `[1,2]`,
			[]string{"raw", "--dry-run", "--input", "-"}},
		{"unknown flag", validRequest,
			[]string{"raw", "--nope", "--input", "-"}},
		{"bad log level", validRequest,
			[]string{"raw", "--dry-run", "--log-level", "loud",
				"--input", "-"}},
		{"positional arg", validRequest,
			[]string{"raw", "--dry-run", "--input", "-", "extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			noKey(t)

			got := invoke(t, tt.stdin, tt.args...)
			if got.code != exitUsage {
				t.Errorf("code = %d, want %d (stderr %s)",
					got.code, exitUsage, got.stderr)
			}
			if ty := errType(t, got.stderr); ty != "usage" {
				t.Errorf("error type = %q, want usage", ty)
			}
		})
	}
}

// ── Requests ────────────────────────────────────────────────────────────────

func TestRawNoKeyMakesNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			t.Error("server was called without a key")
		},
	))
	defer srv.Close()

	noKey(t)
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)

	got := invoke(t, validRequest, "raw", "--input", "-")
	if got.code != exitAuth {
		t.Errorf("code = %d, want %d", got.code, exitAuth)
	}
	if ty := errType(t, got.stderr); ty != "auth" {
		t.Errorf("error type = %q, want auth", ty)
	}
}

func TestRawExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode int
		wantType string
	}{
		{"401", 401, "no", exitAuth, "auth"},
		{"422", 422, "bad state", exitRequest, "request"},
		{"429", 429, "slow down", exitRateLimit, "rate_limit"},
		{"500", 500, "boom", exitUpstream, "server"},
		{"529", 529, "overloaded", exitUpstream, "server"},
		{"200 invalid", 200, "not json", exitUpstream, "response"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, tt.body)
				},
			))
			defer srv.Close()

			withKey(t, "sekret")
			t.Setenv("TYPESAFE_BASE_URL", srv.URL)

			got := invoke(t, validRequest, "raw", "--input", "-")
			if got.code != tt.wantCode {
				t.Errorf("code = %d, want %d (stderr %s)",
					got.code, tt.wantCode, got.stderr)
			}
			if ty := errType(t, got.stderr); ty != tt.wantType {
				t.Errorf("error type = %q, want %q", ty, tt.wantType)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
		})
	}
}

func TestRawNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {},
	))
	url := srv.URL
	srv.Close()

	withKey(t, "sekret")
	t.Setenv("TYPESAFE_BASE_URL", url)

	got := invoke(t, validRequest, "raw", "--input", "-")
	if got.code != exitNetwork {
		t.Errorf("code = %d, want %d (stderr %s)",
			got.code, exitNetwork, got.stderr)
	}
	if ty := errType(t, got.stderr); ty != "network" {
		t.Errorf("error type = %q, want network", ty)
	}
}

func TestRawSuccessGolden(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = io.WriteString(w, fixedResponse)
		},
	))
	defer srv.Close()

	withKey(t, "sekret")
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)
	t.Setenv("TYPESAFE_DEFAULT_MODEL", "jev-test")

	got := invoke(t, validRequest, "raw", "--input", "-")
	if got.code != 0 {
		t.Fatalf("code = %d, stderr = %s", got.code, got.stderr)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want empty", got.stderr)
	}
	if got.stdout != fixedResponse+"\n" {
		t.Errorf("stdout = %q, want %q", got.stdout, fixedResponse+"\n")
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["model"] != "jev-test" {
		t.Errorf("request model = %v, want jev-test", gotBody["model"])
	}

	if strings.Contains(got.stdout, "\r") {
		t.Error("stdout contains a carriage return")
	}
	if strings.HasPrefix(got.stdout, "\ufeff") {
		t.Error("stdout starts with a BOM")
	}
}

func TestRawInputFromFile(t *testing.T) {
	noKey(t)
	t.Setenv("TYPESAFE_DEFAULT_MODEL", "jev-test")

	path := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(
		path, []byte(validRequest), 0o600,
	); err != nil {
		t.Fatalf("write request: %v", err)
	}

	got := invoke(t, "", "raw", "--dry-run", "--input", path)
	if got.code != 0 {
		t.Fatalf("code = %d, stderr = %s", got.code, got.stderr)
	}

	got = invoke(t, "", "raw", "--dry-run", "--input", path+".absent")
	if got.code != exitUsage {
		t.Errorf("code = %d, want %d", got.code, exitUsage)
	}
}

// ── session agent kinds ─────────────────────────────────────────────────────

// TestSessionAcceptsEveryAgentKind checks the five accepted --agent
// values through --dry-run, which needs no key and no network.
func TestSessionAcceptsEveryAgentKind(t *testing.T) {
	fixture := filepath.Join(
		"..", "session", "testdata", "unsubmitted-04.txt",
	)

	for _, kind := range []string{
		"claude", "codex", "pi", "opencode", "unknown",
	} {
		t.Run(kind, func(t *testing.T) {
			noKey(t)

			got := invoke(t, "", "session", "--dry-run",
				"--input", fixture, "--agent", kind)
			if got.code != 0 {
				t.Fatalf("code = %d, stderr = %s",
					got.code, got.stderr)
			}

			body, ok := dryRunBody(t, got.stdout)["body"].(map[string]any)
			if !ok {
				t.Fatalf("no body in %s", got.stdout)
			}
			state, ok := body["state"].(map[string]any)
			if !ok {
				t.Fatalf("no state in %s", got.stdout)
			}
			if state["agent_kind"] != kind {
				t.Errorf("agent_kind = %v, want %s",
					state["agent_kind"], kind)
			}
		})
	}
}

// TestSessionRejectsOtherAgentKinds covers the usage error: it exits
// 2 and its message names all five accepted values.
func TestSessionRejectsOtherAgentKinds(t *testing.T) {
	noKey(t)

	fixture := filepath.Join(
		"..", "session", "testdata", "idle-05.txt",
	)

	got := invoke(t, "", "session", "--dry-run",
		"--input", fixture, "--agent", "gemini")
	if got.code != exitUsage {
		t.Fatalf("code = %d, want %d (stderr %s)",
			got.code, exitUsage, got.stderr)
	}
	if ty := errType(t, got.stderr); ty != "usage" {
		t.Errorf("error type = %q, want usage", ty)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	for _, kind := range []string{
		"claude", "codex", "pi", "opencode", "unknown",
	} {
		if !strings.Contains(got.stderr, kind) {
			t.Errorf("stderr does not name %q: %s",
				kind, got.stderr)
		}
	}
}

// ── session request parity ──────────────────────────────────────────────────

// sessionVerdict is the body the session server returns.
const sessionVerdict = `{"model":"m9","answers":{` +
	`"state":{"type":"choice","choice":"unsubmitted",` +
	`"confidence":0.8,"probabilities":{"unsubmitted":0.8}},` +
	`"coherent":{"type":"noul","noul":0.9}},` +
	`"usage":{"input_tokens":1,"output_tokens":1}}`

// TestSessionRequestParity covers R3-02, extended by R7-01 to every
// agent kind with a real capture: the body the server receives
// carries the cleaned tail, the extracted input line, the agent kind,
// and the model override; the verdict echoes the same input line; and
// --dry-run prints exactly the body that was sent.
// sessionParity is one row of the session parity table: a fixture,
// the agent kind that drew it, the input line the request must carry,
// and a substring of the activity hint it must carry ("" for none).
type sessionParity struct {
	kind, fixture, wantInput, wantHint string
}

func TestSessionRequestParity(t *testing.T) {
	tests := []sessionParity{
		{
			kind:      "claude",
			fixture:   "ambiguous-01.txt",
			wantInput: "go ahead and stub resources/tmux.md",
		},
		{
			kind:      "pi",
			fixture:   "unsubmitted-04.txt",
			wantInput: "add a regression test for the empty case",
		},
		{kind: "pi", fixture: "idle-05.txt"},
		{
			kind:      "opencode",
			fixture:   "unsubmitted-03.txt",
			wantInput: "refactor the parser to use a state table",
		},
		{kind: "opencode", fixture: "idle-04.txt"},
		{
			kind:      "opencode",
			fixture:   "unsubmitted-05.txt",
			wantInput: "now add a unit test for greet",
		},
		{kind: "opencode", fixture: "idle-06.txt"},
		{
			kind:     "pi",
			fixture:  "working-04.txt",
			wantHint: "Working",
		},
		{
			kind:     "opencode",
			fixture:  "working-05.txt",
			wantHint: "esc interrupt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.kind+"/"+tt.fixture, func(t *testing.T) {
			checkSessionParity(t, tt)
		})
	}
}

// checkSessionParity runs the session verb against a local server for
// one fixture and agent kind, and checks the request, the verdict,
// and the --dry-run body against each other.
func checkSessionParity(t *testing.T, tt sessionParity) {
	t.Helper()

	kind, wantInput := tt.kind, tt.wantInput
	fixture := filepath.Join("..", "session", "testdata", tt.fixture)
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	wantTail := session.Clean(
		string(raw), session.MaxLines, session.MaxBytes,
	)

	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&sent)
			_, _ = io.WriteString(w, sessionVerdict)
		},
	))
	defer srv.Close()

	withKey(t, "sekret")
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)

	args := []string{
		"session", "--input", fixture, "--agent", kind,
		"--model", "m9",
	}

	got := invoke(t, "", args...)
	if got.code != 0 {
		t.Fatalf("code = %d, stderr = %s", got.code, got.stderr)
	}

	if sent["model"] != "m9" {
		t.Errorf("model = %v, want m9", sent["model"])
	}
	state, ok := sent["state"].(map[string]any)
	if !ok {
		t.Fatalf("state is %T", sent["state"])
	}
	if state["agent_kind"] != kind {
		t.Errorf("agent_kind = %v, want %s", state["agent_kind"], kind)
	}
	if state["transcript_tail"] != wantTail {
		t.Errorf("transcript_tail is not session.Clean of the fixture")
	}
	if state["input_line"] != wantInput {
		t.Errorf("sent input_line = %q, want %q",
			state["input_line"], wantInput)
	}
	hint, ok := state["activity_hint"].(string)
	if !ok {
		t.Fatalf("activity_hint is %T", state["activity_hint"])
	}
	if !strings.Contains(hint, tt.wantHint) ||
		(tt.wantHint == "" && hint != "") {
		t.Errorf("sent activity_hint = %q, want %q in it",
			hint, tt.wantHint)
	}

	var verdict session.Verdict
	if err := json.Unmarshal([]byte(got.stdout), &verdict); err != nil {
		t.Fatalf("stdout %q is not a verdict: %v", got.stdout, err)
	}
	if verdict.InputLine != wantInput {
		t.Errorf("verdict input_line = %q, want %q",
			verdict.InputLine, wantInput)
	}
	if verdict.ActivityHint != hint {
		t.Errorf("verdict activity_hint = %q, want the sent %q",
			verdict.ActivityHint, hint)
	}

	dry := invoke(t, "", append(args, "--dry-run")...)
	if dry.code != 0 {
		t.Fatalf("dry-run code = %d, stderr = %s",
			dry.code, dry.stderr)
	}
	if body := dryRunBody(t, dry.stdout)["body"]; !reflect.DeepEqual(
		body, sent,
	) {
		t.Errorf("--dry-run body differs from the request sent:"+
			"\n dry %v\nsent %v", body, sent)
	}
}

// taskAnswers is the body the task parity server returns.
const taskAnswers = `{"model":"jev-1","answers":{` +
	`"needs_interpretation":{"type":"noul","noul":0.07},` +
	`"acceptance_strength":{"type":"score","score":2.54,` +
	`"confidence":0.83},` +
	`"scope_generic":{"type":"noul","noul":0.04}},` +
	`"usage":{"input_tokens":1400,"output_tokens":9}}`

func TestTaskRequestParity(t *testing.T) {
	path := filepath.Join(
		"..", "tasklint", "testdata", "good-01.md",
	)

	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&sent)
			_, _ = io.WriteString(w, taskAnswers)
		},
	))
	defer srv.Close()

	withKey(t, "sekret")
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)

	got := invoke(t, "", "task", "--model", "m9", path)
	if got.code != 0 {
		t.Fatalf("code = %d, stderr = %s", got.code, got.stderr)
	}

	report := dryRunBody(t, got.stdout)
	if report["file"] != path {
		t.Errorf("file = %v, want %s", report["file"], path)
	}
	judgments, ok := report["judgments"].(map[string]any)
	if !ok {
		t.Fatalf("judgments is %T", report["judgments"])
	}
	if judgments["needs_interpretation"] != 0.07 {
		t.Errorf("judgments = %v", judgments)
	}
	acceptance, ok := report["acceptance"].(map[string]any)
	if !ok {
		t.Fatalf("acceptance is %T", report["acceptance"])
	}
	if acceptance["score"] != 2.54 || acceptance["confidence"] != 0.83 {
		t.Errorf("acceptance = %v", acceptance)
	}

	dry := invoke(t, "", "task", "--model", "m9", "--dry-run", path)
	if dry.code != 0 {
		t.Fatalf("dry-run code = %d, stderr = %s",
			dry.code, dry.stderr)
	}
	if body := dryRunBody(t, dry.stdout)["body"]; !reflect.DeepEqual(
		body, sent,
	) {
		t.Errorf("--dry-run body differs from the request sent:"+
			"\n dry %v\nsent %v", body, sent)
	}
}

// ── review request parity ───────────────────────────────────────────────────

// reviewAnswers is the body the review parity server returns. Every
// item in round-02.md has at most one response.
const reviewAnswers = `{"model":"jev-1","answers":{` +
	`"style_only":{"type":"noul","noul":0.11},` +
	`"response_0":{"type":"choice","choice":"fixed",` +
	`"confidence":0.9,"probabilities":{"fixed":0.9}}},` +
	`"usage":{"input_tokens":700,"output_tokens":4}}`

// TestReviewRequestParity pins the carry-forward rule: the bodies the
// server receives are exactly the ones --dry-run prints, one per
// item, in order.
func TestReviewRequestParity(t *testing.T) {
	path := filepath.Join(
		"..", "review", "testdata", "round-02.md",
	)

	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent = append(sent, body)
			_, _ = io.WriteString(w, reviewAnswers)
		},
	))
	defer srv.Close()

	withKey(t, "sekret")
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)

	got := invoke(t, "", "review", "--model", "m9", path)
	if got.code != 0 {
		t.Fatalf("code = %d, stderr = %s", got.code, got.stderr)
	}

	lines := strings.Split(strings.TrimRight(got.stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout has %d lines, want 3:\n%s",
			len(lines), got.stdout)
	}
	if len(sent) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(sent))
	}
	for i, want := range []string{"R1-01", "R1-02", "R1-03"} {
		if id := dryRunBody(t, lines[i])["id"]; id != want {
			t.Errorf("line %d is for %v, want %s", i, id, want)
		}
	}

	dry := invoke(t, "", "review", "--model", "m9", "--dry-run", path)
	if dry.code != 0 {
		t.Fatalf("dry-run code = %d, stderr = %s",
			dry.code, dry.stderr)
	}

	dryLines := strings.Split(
		strings.TrimRight(dry.stdout, "\n"), "\n",
	)
	if len(dryLines) != len(sent) {
		t.Fatalf("--dry-run printed %d records, %d were sent",
			len(dryLines), len(sent))
	}
	for i, line := range dryLines {
		if body := dryRunBody(t, line)["body"]; !reflect.DeepEqual(
			body, sent[i],
		) {
			t.Errorf("item %d: --dry-run body differs from the "+
				"request sent:\n dry %v\nsent %v",
				i, body, sent[i])
		}
	}
}

// TestReviewZeroItemsNeedsNoKey is R5-02: a round with no items must
// print nothing and exit 0, without reaching for a credential.
func TestReviewZeroItemsNeedsNoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			t.Error("a zero-item round made a request")
		},
	))
	defer srv.Close()

	for _, tt := range []struct {
		name, stdin string
	}{
		{"empty", ""},
		{"no items", "# Round 7\n\nNone.\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			noKey(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("TYPESAFE_BASE_URL", srv.URL)

			got := invoke(t, tt.stdin, "review", "-")
			if got.code != 0 {
				t.Errorf("code = %d, want 0 (stderr %s)",
					got.code, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if got.stderr != "" {
				t.Errorf("stderr = %q, want empty", got.stderr)
			}
		})
	}
}

func TestReviewRejectsPretty(t *testing.T) {
	noKey(t)

	path := filepath.Join(
		"..", "review", "testdata", "round-02.md",
	)

	got := invoke(t, "", "review", "--pretty", "--dry-run", path)
	if got.code != exitUsage {
		t.Errorf("code = %d, want %d (stderr %s)",
			got.code, exitUsage, got.stderr)
	}
	if ty := errType(t, got.stderr); ty != "usage" {
		t.Errorf("error type = %q, want usage", ty)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
}

// ── Exit-code map ───────────────────────────────────────────────────────────

func TestCodeForUnknown(t *testing.T) {
	if got := codeFor(io.ErrUnexpectedEOF); got != exitUnknown {
		t.Errorf("codeFor = %d, want %d", got, exitUnknown)
	}
}

func TestCodeForInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := codeFor(ctx.Err()); got != exitInterrupt {
		t.Errorf("codeFor = %d, want %d", got, exitInterrupt)
	}
}

// ── Round 1 fixes ───────────────────────────────────────────────────────────

func TestRawRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"null", "null"},
		{"empty object", "{}"},
		{"null answers", `{"model":"m","answers":null}`},
		{"trailing document", fixedResponse + " {}"},
		{"not json", "not json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, tt.body)
				},
			))
			defer srv.Close()

			withKey(t, "sekret")
			t.Setenv("TYPESAFE_BASE_URL", srv.URL)

			got := invoke(t, validRequest, "raw", "--input", "-")
			if got.code != exitUpstream {
				t.Errorf("code = %d, want %d (stderr %s)",
					got.code, exitUpstream, got.stderr)
			}
			if ty := errType(t, got.stderr); ty != "response" {
				t.Errorf("error type = %q, want response", ty)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
		})
	}
}

func TestEchoedKeyNeverReachesAnyStream(t *testing.T) {
	const key = "fake-key-abc123"

	tests := []struct {
		name   string
		status int
	}{
		{"request error", 422},
		{"server error", 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					// Echo the credential back, as a careless
					// upstream diagnostic would.
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(
						w, "rejected: "+r.Header.Get("Authorization"),
					)
				},
			))
			defer srv.Close()

			withKey(t, key)
			t.Setenv("TYPESAFE_BASE_URL", srv.URL)

			got := invoke(t, validRequest,
				"raw", "--log-level", "debug", "--input", "-")
			if got.code == 0 {
				t.Fatal("want a non-zero exit")
			}
			if strings.Contains(got.stdout, key) {
				t.Errorf("stdout leaks the key: %q", got.stdout)
			}
			if strings.Contains(got.stderr, key) {
				t.Errorf("stderr leaks the key: %q", got.stderr)
			}
			if !strings.Contains(got.stderr, "[redacted]") {
				t.Errorf("stderr lacks the placeholder: %q",
					got.stderr)
			}
		})
	}
}

func TestEveryFailureEndsWithOneErrorRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	))
	defer srv.Close()

	cases := []struct {
		name  string
		stdin string
		args  []string
		setup func(t *testing.T)
	}{
		{name: "no args", setup: noKey},
		{
			name:  "unknown verb",
			args:  []string{"nope"},
			setup: noKey,
		},
		{
			name:  "usage",
			stdin: "null",
			args:  []string{"raw", "--dry-run", "--input", "-"},
			setup: noKey,
		},
		{
			name:  "no key",
			stdin: validRequest,
			args:  []string{"raw", "--input", "-"},
			setup: noKey,
		},
		{
			name:  "auth",
			stdin: validRequest,
			args:  []string{"raw", "--input", "-"},
			setup: func(t *testing.T) {
				withKey(t, "sekret")
				t.Setenv("TYPESAFE_BASE_URL", srv.URL)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)

			got := invoke(t, tc.stdin, tc.args...)
			if got.code == 0 {
				t.Fatalf("code = 0, want non-zero")
			}

			lines := strings.Split(
				strings.TrimRight(got.stderr, "\n"), "\n",
			)
			records := 0
			for _, line := range lines {
				var rec errorRecord
				if json.Unmarshal([]byte(line), &rec) == nil &&
					rec.Error.Type != "" {
					records++
				}
			}
			if records != 1 {
				t.Errorf("stderr holds %d error records, want 1:\n%s",
					records, got.stderr)
			}
			if errType(t, got.stderr) == "" {
				t.Error("the last stderr line is not an error record")
			}
		})
	}
}

// ── Round 2 fixes ───────────────────────────────────────────────────────────

// rawServer serves one hand-written HTTP response on a local socket,
// so a test can send a malformed head or stall with bytes still
// outstanding. It returns the base URL and a channel closed once the
// response bytes have been written.
func rawServer(t *testing.T, head, body string) (string, <-chan struct{}) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan struct{})
	sent := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Consume the request head so the client's write completes.
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}

		_, _ = io.WriteString(conn, head)
		_, _ = io.WriteString(conn, body)
		close(sent)

		// Hold the connection open so the client stalls.
		<-done
	}()

	return "http://" + ln.Addr().String(), sent
}

func TestInterruptDuringDrainReportsInterrupted(t *testing.T) {
	// The reviewer's recipe: 422 declaring 1000 bytes, 500 sent, then
	// a stall. readErrBody takes its 500 diagnostic bytes and the
	// drain blocks on the rest, after the RequestError was chosen.
	url, sent := rawServer(t,
		"HTTP/1.1 422 Unprocessable Entity\r\n"+
			"Content-Length: 1000\r\n\r\n",
		strings.Repeat("x", 500))

	withKey(t, "sekret")
	t.Setenv("TYPESAFE_BASE_URL", url)

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut bytes.Buffer
	codes := make(chan int, 1)
	go func() {
		codes <- Run(
			ctx,
			[]string{"raw", "--input", "-", "--log-level", "debug"},
			strings.NewReader(validRequest), &out, &errOut,
		)
	}()

	<-sent
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case code := <-codes:
		if code != exitInterrupt {
			t.Errorf("code = %d, want %d (stderr %s)",
				code, exitInterrupt, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	if ty := errType(t, errOut.String()); ty != "interrupted" {
		t.Errorf("error type = %q, want interrupted", ty)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestInterruptDuringBodyDrainOn200(t *testing.T) {
	// The same defect on the 200 path: invalid JSON is read, the
	// ResponseError is chosen, then the drain blocks.
	url, sent := rawServer(t,
		"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n",
		"x"+strings.Repeat(" ", 499))

	withKey(t, "sekret")
	t.Setenv("TYPESAFE_BASE_URL", url)

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut bytes.Buffer
	codes := make(chan int, 1)
	go func() {
		codes <- Run(
			ctx, []string{"raw", "--input", "-"},
			strings.NewReader(validRequest), &out, &errOut,
		)
	}()

	<-sent
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case code := <-codes:
		if code != exitInterrupt {
			t.Errorf("code = %d, want %d (stderr %s)",
				code, exitInterrupt, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	if ty := errType(t, errOut.String()); ty != "interrupted" {
		t.Errorf("error type = %q, want interrupted", ty)
	}
}

func TestMalformedContentLengthDoesNotLeakKey(t *testing.T) {
	// The reviewer's R1-06 residual: the key reaches stderr through
	// an HTTP parser error, which carries upstream text but must keep
	// its NetworkError type and wrapped cause.
	const key = "round2-fake-token"

	url, _ := rawServer(t,
		"HTTP/1.1 200 OK\r\nContent-Length: "+key+"\r\n\r\n", "")

	withKey(t, key)
	t.Setenv("TYPESAFE_BASE_URL", url)

	got := invoke(t, validRequest,
		"raw", "--log-level", "debug", "--input", "-")
	if got.code != exitNetwork {
		t.Errorf("code = %d, want %d (stderr %s)",
			got.code, exitNetwork, got.stderr)
	}
	if ty := errType(t, got.stderr); ty != "network" {
		t.Errorf("error type = %q, want network", ty)
	}
	if strings.Contains(got.stdout, key) {
		t.Errorf("stdout leaks the key: %q", got.stdout)
	}
	if strings.Contains(got.stderr, key) {
		t.Errorf("stderr leaks the key: %q", got.stderr)
	}
	if !strings.Contains(got.stderr, "[redacted]") {
		t.Errorf("stderr lacks the placeholder: %q", got.stderr)
	}
}

func TestRedactAttr(t *testing.T) {
	g := &globals{apiKey: "sekret"}
	fn := redactAttr(g.redact)

	t.Run("string attribute", func(t *testing.T) {
		got := fn(nil, slog.String("body", "Bearer sekret echoed"))
		if got.Value.String() != "Bearer [redacted] echoed" {
			t.Errorf("value = %q", got.Value.String())
		}
	})

	t.Run("message", func(t *testing.T) {
		got := fn(nil, slog.String(slog.MessageKey, "sent sekret"))
		if strings.Contains(got.Value.String(), "sekret") {
			t.Errorf("message leaks the key: %q", got.Value.String())
		}
	})

	t.Run("non-string attribute is untouched", func(t *testing.T) {
		got := fn(nil, slog.Int("bytes", 7))
		if got.Value.Kind() != slog.KindInt64 ||
			got.Value.Int64() != 7 {
			t.Errorf("value = %v", got.Value)
		}
	})
}

func TestRedactBeforeKeyIsResolved(t *testing.T) {
	var g globals
	if got := g.redact("nothing yet"); got != "nothing yet" {
		t.Errorf("redact = %q, want the input unchanged", got)
	}

	g.apiKey = "sekret"
	if got := g.redact("a sekret"); got != "a [redacted]" {
		t.Errorf("redact = %q", got)
	}
}
