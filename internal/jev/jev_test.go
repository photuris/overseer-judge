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
