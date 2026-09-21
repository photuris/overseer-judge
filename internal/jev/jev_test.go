package jev

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// okBody is a response exercising every answer field.
const okBody = `{
	"model": "jev-latest",
	"answers": {
		"n": {"type": "noul", "noul": 0.75},
		"c": {
			"type": "choice",
			"choice": "idle",
			"probabilities": {"idle": 0.8, "working": 0.2},
			"confidence": 0.8,
			"legend": {"idle": "nothing happening"}
		},
		"s": {"type": "score", "score": 3.5}
	},
	"usage": {"input_tokens": 12, "output_tokens": 3}
}`

// newClient returns a Client pointed at url with instant sleeps and
// the number of sleeps recorded.
func newClient(url string) (*Client, *int) {
	c := New(&http.Client{}, url, "sekret", "jev-default")
	sleeps := 0
	c.sleep = func(context.Context, time.Duration) error {
		sleeps++

		return nil
	}

	return c, &sleeps
}

// bodyOf marshals a minimal valid request body.
func bodyOf(t *testing.T, req Request) []byte {
	t.Helper()

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

func TestAskRawSuccess(t *testing.T) {
	var gotAuth, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotType = r.Header.Get("Content-Type")
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			if r.URL.Path != Path {
				t.Errorf("path = %q, want %q", r.URL.Path, Path)
			}
			_, _ = io.WriteString(w, okBody)
		},
	))
	defer srv.Close()

	c, _ := newClient(srv.URL)
	resp, err := c.AskRaw(
		context.Background(), []byte(`{"state":"x"}`),
	)
	if err != nil {
		t.Fatalf("AskRaw: %v", err)
	}

	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q", gotType)
	}
	if gotBody != `{"state":"x"}` {
		t.Errorf("body = %q, want it sent verbatim", gotBody)
	}
	if resp.Model != "jev-latest" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if n := resp.Answers["n"].Noul; n == nil || *n != 0.75 {
		t.Errorf("noul = %v", n)
	}
	if s := resp.Answers["s"].Score; s == nil || *s != 3.5 {
		t.Errorf("score = %v", s)
	}
	a := resp.Answers["c"]
	if a.Choice != "idle" || a.Probabilities["working"] != 0.2 {
		t.Errorf("choice answer = %+v", a)
	}
	if a.Confidence == nil || *a.Confidence != 0.8 {
		t.Errorf("confidence = %v", a.Confidence)
	}
	if a.Legend["idle"] != "nothing happening" {
		t.Errorf("legend = %v", a.Legend)
	}
}

func TestAskFillsModel(t *testing.T) {
	var got Request
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = io.WriteString(w, okBody)
		},
	))
	defer srv.Close()

	c, _ := newClient(srv.URL)
	if _, err := c.Ask(context.Background(), Request{
		State:     "x",
		Questions: map[string]Question{"n": {Type: "noul"}},
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Model != "jev-default" {
		t.Errorf("Model = %q, want the client default", got.Model)
	}

	if _, err := c.Ask(context.Background(), Request{
		State: "x", Model: "explicit",
		Questions: map[string]Question{"n": {Type: "noul"}},
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Model != "explicit" {
		t.Errorf("Model = %q, want the request's own", got.Model)
	}
}

func TestAskRawStatuses(t *testing.T) {
	tests := []struct {
		name string
		// statuses is one entry per attempt the server should serve;
		// 200 serves okBody.
		statuses   []int
		retryAfter string
		// body replaces okBody on a 200 response.
		body     string
		wantReqs int
		check    func(t *testing.T, resp *Response, err error)
	}{
		{
			name:     "401 auth",
			statuses: []int{401},
			wantReqs: 1,
			check: func(t *testing.T, _ *Response, err error) {
				var e *AuthError
				if !errors.As(err, &e) || e.Status != 401 {
					t.Fatalf("err = %v, want AuthError 401", err)
				}
			},
		},
		{
			name:     "422 request",
			statuses: []int{422},
			wantReqs: 1,
			check: func(t *testing.T, _ *Response, err error) {
				var e *RequestError
				if !errors.As(err, &e) {
					t.Fatalf("err = %v, want RequestError", err)
				}
				if e.Status != 422 {
					t.Errorf("Status = %d, want 422", e.Status)
				}
				if !strings.Contains(e.Message, "status 422 body") {
					t.Errorf("Message = %q, want the body", e.Message)
				}
			},
		},
		{
			name:     "429 exhausts retries",
			statuses: []int{429, 429, 429},
			wantReqs: 3,
			check: func(t *testing.T, _ *Response, err error) {
				var e *RateLimitError
				if !errors.As(err, &e) || e.Status != 429 {
					t.Fatalf("err = %v, want RateLimitError", err)
				}
			},
		},
		{
			name:     "429 then 200",
			statuses: []int{429, 200},
			wantReqs: 2,
			check: func(t *testing.T, resp *Response, err error) {
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				if resp.Model != "jev-latest" {
					t.Errorf("Model = %q", resp.Model)
				}
			},
		},
		{
			name:     "500 then 200",
			statuses: []int{500, 200},
			wantReqs: 2,
			check: func(t *testing.T, _ *Response, err error) {
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
			},
		},
		{
			name:       "503 with Retry-After 0 then 200",
			statuses:   []int{503, 200},
			retryAfter: "0",
			wantReqs:   2,
			check: func(t *testing.T, _ *Response, err error) {
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
			},
		},
		{
			name:     "500 exhausts retries",
			statuses: []int{500, 500, 500},
			wantReqs: 3,
			check: func(t *testing.T, _ *Response, err error) {
				var e *ServerError
				if !errors.As(err, &e) || e.Status != 500 {
					t.Fatalf("err = %v, want ServerError 500", err)
				}
			},
		},
		{
			name:     "529 exhausts retries",
			statuses: []int{529, 529, 529},
			wantReqs: 3,
			check: func(t *testing.T, _ *Response, err error) {
				var e *ServerError
				if !errors.As(err, &e) || e.Status != 529 {
					t.Fatalf("err = %v, want ServerError 529", err)
				}
			},
		},
		{
			name:     "200 with invalid body",
			statuses: []int{200},
			body:     "not json",
			wantReqs: 1,
			check: func(t *testing.T, _ *Response, err error) {
				var e *ResponseError
				if !errors.As(err, &e) {
					t.Fatalf("err = %v, want ResponseError", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs := 0
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					status := tt.statuses[min(reqs, len(tt.statuses)-1)]
					reqs++
					if status == http.StatusOK {
						_, _ = io.WriteString(
							w, cmp.Or(tt.body, okBody),
						)

						return
					}
					if tt.retryAfter != "" {
						w.Header().Set("Retry-After", tt.retryAfter)
					}
					w.WriteHeader(status)
					_, _ = io.WriteString(
						w, "status "+strconv.Itoa(status)+" body",
					)
				},
			))
			defer srv.Close()

			c, _ := newClient(srv.URL)
			resp, err := c.AskRaw(
				context.Background(), bodyOf(t, Request{State: "x"}),
			)
			tt.check(t, resp, err)
			if reqs != tt.wantReqs {
				t.Errorf("requests = %d, want %d", reqs, tt.wantReqs)
			}
		})
	}
}

func TestAskRawRetryAfterOverridesWait(t *testing.T) {
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			reqs++
			if reqs == 1 {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)

				return
			}
			_, _ = io.WriteString(w, okBody)
		},
	))
	defer srv.Close()

	var waited []time.Duration
	c := New(&http.Client{}, srv.URL, "sekret", "jev-default")
	c.sleep = func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)

		return nil
	}

	if _, err := c.AskRaw(
		context.Background(), bodyOf(t, Request{State: "x"}),
	); err != nil {
		t.Fatalf("AskRaw: %v", err)
	}
	if len(waited) != 1 || waited[0] != 7*time.Second {
		t.Errorf("waits = %v, want [7s]", waited)
	}
}

func TestAskRawDefaultWaits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		},
	))
	defer srv.Close()

	var waited []time.Duration
	c := New(&http.Client{}, srv.URL, "sekret", "jev-default")
	c.sleep = func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)

		return nil
	}

	if _, err := c.AskRaw(
		context.Background(), bodyOf(t, Request{State: "x"}),
	); err == nil {
		t.Fatal("AskRaw: want an error")
	}
	want := []time.Duration{500 * time.Millisecond, time.Second}
	if len(waited) != 2 || waited[0] != want[0] || waited[1] != want[1] {
		t.Errorf("waits = %v, want %v", waited, want)
	}
}

func TestAskRawNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {},
	))
	url := srv.URL
	srv.Close()

	c, _ := newClient(url)
	_, err := c.AskRaw(
		context.Background(), bodyOf(t, Request{State: "x"}),
	)
	var e *NetworkError
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want NetworkError", err)
	}
}

func TestAskRawContextCancelledDuringSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		},
	))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New(&http.Client{}, srv.URL, "sekret", "jev-default")
	c.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()

		return ctx.Err()
	}

	_, err := c.AskRaw(ctx, bodyOf(t, Request{State: "x"}))
	var e *NetworkError
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want NetworkError", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestNoulAccessor(t *testing.T) {
	p := 0.6
	resp := &Response{Answers: map[string]Answer{
		"ok":    {Type: "noul", Noul: &p},
		"wrong": {Type: "choice", Choice: "a"},
		"nil":   {Type: "noul"},
	}}

	got, err := resp.Noul("ok")
	if err != nil || got != 0.6 {
		t.Fatalf("Noul(ok) = %v, %v", got, err)
	}

	for _, id := range []string{"missing", "wrong", "nil"} {
		t.Run(id, func(t *testing.T) {
			_, err := resp.Noul(id)
			var e *ResponseError
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want ResponseError", err)
			}
		})
	}
}

func TestChoiceAccessor(t *testing.T) {
	conf := 0.9
	probs := map[string]float64{"idle": 0.9, "working": 0.1}
	resp := &Response{Answers: map[string]Answer{
		"ok": {
			Type: "choice", Choice: "idle",
			Probabilities: probs, Confidence: &conf,
		},
		"wrong": {Type: "noul"},
		"empty": {
			Type: "choice", Probabilities: probs, Confidence: &conf,
		},
		"noconf": {
			Type: "choice", Choice: "idle", Probabilities: probs,
		},
		"noprobs": {
			Type: "choice", Choice: "idle", Confidence: &conf,
		},
		"unlisted": {
			Type: "choice", Choice: "nope",
			Probabilities: probs, Confidence: &conf,
		},
	}}

	choice, confidence, gotProbs, err := resp.Choice("ok")
	if err != nil {
		t.Fatalf("Choice(ok): %v", err)
	}
	if choice != "idle" || confidence != 0.9 {
		t.Errorf("Choice(ok) = %q, %v", choice, confidence)
	}
	if gotProbs["working"] != 0.1 {
		t.Errorf("probabilities = %v", gotProbs)
	}

	bad := []string{
		"missing", "wrong", "empty", "noconf", "noprobs", "unlisted",
	}
	for _, id := range bad {
		t.Run(id, func(t *testing.T) {
			_, _, _, err := resp.Choice(id)
			var e *ResponseError
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want ResponseError", err)
			}
		})
	}
}

func TestScoreAccessor(t *testing.T) {
	score, conf := 2.54, 0.88
	resp := &Response{Answers: map[string]Answer{
		"ok":     {Type: "score", Score: &score, Confidence: &conf},
		"wrong":  {Type: "noul", Score: &score, Confidence: &conf},
		"nil":    {Type: "score", Confidence: &conf},
		"noconf": {Type: "score", Score: &score},
	}}

	got, confidence, err := resp.Score("ok")
	if err != nil {
		t.Fatalf("Score(ok): %v", err)
	}
	if got != 2.54 || confidence != 0.88 {
		t.Errorf("Score(ok) = %v, %v, want 2.54, 0.88",
			got, confidence)
	}

	for _, id := range []string{"missing", "wrong", "nil", "noconf"} {
		t.Run(id, func(t *testing.T) {
			_, _, err := resp.Score(id)
			var e *ResponseError
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want ResponseError", err)
			}
		})
	}
}

// ── Response body handling (R1-01, R1-02, R1-03, R1-06) ─────────────────────

// roundTripFunc adapts a function to http.RoundTripper so a test can
// hand back an exact body.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// trackingBody records how a response body was consumed.
type trackingBody struct {
	r io.Reader
	// readErr replaces io.EOF once r is exhausted, to simulate a
	// transport failure part-way through a body.
	readErr error
	// hook runs before the replacement error is returned.
	hook   func()
	sawEOF bool
	closed bool
}

// Read implements io.Reader.
func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		if b.readErr != nil {
			if b.hook != nil {
				b.hook()
			}

			return n, b.readErr
		}
		b.sawEOF = true
	}

	return n, err
}

// Close implements io.Closer.
func (b *trackingBody) Close() error {
	b.closed = true

	return nil
}

// timeoutErr is a net.Error that reports a timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// stubClient returns a Client whose transport serves the given
// responses in order, plus the bodies it handed out.
func stubClient(
	apiKey string, bodies []*trackingBody, statuses []int,
) (*Client, *[]*trackingBody) {
	served := make([]*trackingBody, 0, len(bodies))
	n := 0
	hc := &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			i := min(n, len(bodies)-1)
			n++
			served = append(served, bodies[i])

			return &http.Response{
				StatusCode: statuses[min(i, len(statuses)-1)],
				Header:     http.Header{"Retry-After": {"0"}},
				Body:       bodies[i],
			}, nil
		},
	)}

	c := New(hc, "http://stub", apiKey, "jev-default")
	c.sleep = func(context.Context, time.Duration) error { return nil }

	return c, &served
}

// body returns a trackingBody serving s.
func body(s string) *trackingBody {
	return &trackingBody{r: strings.NewReader(s)}
}

func TestResponseValidity(t *testing.T) {
	tests := []struct {
		name string
		body string
		// want is the substring expected in a ResponseError, or "" to
		// expect success.
		want string
	}{
		{name: "valid", body: okBody},
		{name: "valid with trailing whitespace", body: okBody + "\n\n "},
		{
			name: "null",
			body: "null",
			want: "null",
		},
		{
			name: "empty object",
			body: "{}",
			want: `no "answers"`,
		},
		{
			name: "null answers",
			body: `{"model":"m","answers":null}`,
			want: "null",
		},
		{
			name: "answers is an array",
			body: `{"model":"m","answers":[]}`,
			want: "decode body",
		},
		{
			name: "not an object",
			body: `[1,2]`,
			want: "not a JSON object",
		},
		{
			name: "trailing document",
			body: okBody + " {}",
			want: "more than one JSON value",
		},
		{
			name: "not json",
			body: "not json",
			want: "decode body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := body(tt.body)
			c, _ := stubClient("k", []*trackingBody{b}, []int{200})

			resp, err := c.AskRaw(context.Background(), []byte(`{}`))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("AskRaw: %v, want success", err)
				}
				if resp.Answers == nil {
					t.Error("Answers is nil on a valid response")
				}

				return
			}

			var e *ResponseError
			if !errors.As(err, &e) {
				t.Fatalf("err = %v (%T), want ResponseError", err, err)
			}
			if !strings.Contains(e.Message, tt.want) {
				t.Errorf("message = %q, want it to mention %q",
					e.Message, tt.want)
			}
			if resp != nil {
				t.Error("a rejected response must not be returned")
			}
		})
	}
}

func TestBodyReadFailuresKeepTheirType(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		b := &trackingBody{
			r:       strings.NewReader(`{"answers":`),
			readErr: timeoutErr{},
		}
		c, _ := stubClient("k", []*trackingBody{b}, []int{200})

		_, err := c.AskRaw(context.Background(), []byte(`{}`))
		var e *NetworkError
		if !errors.As(err, &e) {
			t.Fatalf("err = %v (%T), want NetworkError", err, err)
		}
	})

	t.Run("deadline exceeded", func(t *testing.T) {
		b := &trackingBody{
			r:       strings.NewReader(`{"answers":`),
			readErr: context.DeadlineExceeded,
		}
		c, _ := stubClient("k", []*trackingBody{b}, []int{200})

		_, err := c.AskRaw(context.Background(), []byte(`{}`))
		var e *NetworkError
		if !errors.As(err, &e) {
			t.Fatalf("err = %v (%T), want NetworkError", err, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want it to wrap DeadlineExceeded", err)
		}
	})

	t.Run("interrupted mid-body", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		b := &trackingBody{
			r:       strings.NewReader(`{"answers":`),
			readErr: errors.New("read: connection reset"),
			hook:    cancel,
		}
		c, _ := stubClient("k", []*trackingBody{b}, []int{200})

		_, err := c.AskRaw(ctx, []byte(`{}`))
		var e *NetworkError
		if !errors.As(err, &e) {
			t.Fatalf("err = %v (%T), want NetworkError", err, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to wrap context.Canceled", err)
		}
	})
}

func TestBodiesAreDrainedAndClosed(t *testing.T) {
	// A body longer than one decoded value, so a plain decode leaves
	// bytes behind.
	const padded = okBody + "\n" + `{"ignored":true}`

	tests := []struct {
		name     string
		bodies   []*trackingBody
		statuses []int
	}{
		{
			name:     "success",
			bodies:   []*trackingBody{body(okBody + strings.Repeat(" ", 4096))},
			statuses: []int{200},
		},
		{
			name:     "decode failure",
			bodies:   []*trackingBody{body(padded)},
			statuses: []int{200},
		},
		{
			name: "retry path",
			bodies: []*trackingBody{
				body(strings.Repeat("e", 4096)),
				body(okBody),
			},
			statuses: []int{500, 200},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, served := stubClient("k", tt.bodies, tt.statuses)

			_, _ = c.AskRaw(context.Background(), []byte(`{}`))

			if len(*served) != len(tt.bodies) {
				t.Fatalf("served %d bodies, want %d",
					len(*served), len(tt.bodies))
			}
			for i, b := range *served {
				if !b.closed {
					t.Errorf("body %d was not closed", i)
				}
				if !b.sawEOF {
					t.Errorf("body %d was not drained to EOF", i)
				}
			}
		})
	}
}

func TestEchoedKeyIsRedacted(t *testing.T) {
	const key = "fake-key-abc123"

	tests := []struct {
		name   string
		status int
		// attempts is how many bodies the client will consume; a 5xx
		// is retried, so each attempt needs its own reader.
		attempts int
	}{
		{"request error", 422, 1},
		{"server error", 500, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			echo := "upstream said: Bearer " + key + " is invalid"
			bodies := make([]*trackingBody, tt.attempts)
			for i := range bodies {
				bodies[i] = body(echo)
			}
			c, _ := stubClient(key, bodies, []int{tt.status})

			_, err := c.AskRaw(context.Background(), []byte(`{}`))
			if err == nil {
				t.Fatal("AskRaw: want an error")
			}
			if strings.Contains(err.Error(), key) {
				t.Errorf("error %q leaks the API key", err)
			}
			if !strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("error %q lacks the placeholder", err)
			}
		})
	}
}

func TestEchoedKeyIsRedactedInResponseErrors(t *testing.T) {
	const key = "fake-key-abc123"

	c, _ := stubClient(
		key, []*trackingBody{body(`{"answers":` + key)}, []int{200},
	)

	_, err := c.AskRaw(context.Background(), []byte(`{}`))
	var e *ResponseError
	if !errors.As(err, &e) {
		t.Fatalf("err = %v (%T), want ResponseError", err, err)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error %q leaks the API key", err)
	}
}

func TestRedactSkipsEmptyKey(t *testing.T) {
	c := New(&http.Client{}, "http://stub", "", "jev-default")
	if got := c.redact("nothing to do"); got != "nothing to do" {
		t.Errorf("redact = %q", got)
	}
}
