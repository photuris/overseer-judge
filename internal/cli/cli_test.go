package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
