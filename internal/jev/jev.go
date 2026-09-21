// Package jev is a client for TypeSafe's System One (Jev) API.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Path is the System One endpoint, relative to the base URL.
const Path = "/v1/systemone"

// maxErrBody caps how much of an error response body is kept for
// diagnostics. drainLimit caps how much of a body is discarded before
// closing it, so the connection can be reused.
const (
	maxErrBody = 500
	drainLimit = 1 << 16
)

// retryWaits are the waits before the second and third attempts.
var retryWaits = []time.Duration{500 * time.Millisecond, time.Second}

// Question is one System One question.
type Question struct {
	Type         string `json:"type"` // noul|choice|score
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Request is a System One request body.
type Request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model,omitempty"`
	Questions map[string]Question `json:"questions"`
}

// Answer is one answer from a System One response.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]any     `json:"legend,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// Usage reports the tokens a request consumed.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is a System One response body.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Noul returns the probability for a noul answer. It returns a
// *ResponseError when the answer is missing, its Type is not "noul",
// or Noul is nil.
func (r *Response) Noul(id string) (float64, error) {
	a, ok := r.Answers[id]
	if !ok {
		return 0, &ResponseError{
			Message: fmt.Sprintf("answer %q missing", id),
		}
	}
	if a.Type != "noul" {
		return 0, &ResponseError{Message: fmt.Sprintf(
			"answer %q has type %q, want \"noul\"", id, a.Type,
		)}
	}
	if a.Noul == nil {
		return 0, &ResponseError{
			Message: fmt.Sprintf("answer %q has no noul value", id),
		}
	}

	return *a.Noul, nil
}

// Score returns the score and confidence for a score answer. It
// returns a *ResponseError when the answer is missing, its Type is
// not "score", or Score or Confidence is nil.
func (r *Response) Score(id string) (
	score, confidence float64, err error,
) {
	a, ok := r.Answers[id]
	if !ok {
		return 0, 0, &ResponseError{
			Message: fmt.Sprintf("answer %q missing", id),
		}
	}
	if a.Type != "score" {
		return 0, 0, &ResponseError{Message: fmt.Sprintf(
			"answer %q has type %q, want \"score\"", id, a.Type,
		)}
	}
	if a.Score == nil {
		return 0, 0, &ResponseError{
			Message: fmt.Sprintf("answer %q has no score value", id),
		}
	}
	if a.Confidence == nil {
		return 0, 0, &ResponseError{
			Message: fmt.Sprintf("answer %q has no confidence", id),
		}
	}

	return *a.Score, *a.Confidence, nil
}

// Choice returns the chosen label, confidence, and probabilities for
// a choice answer. It returns a *ResponseError when the answer is
// missing, Type is not "choice", Choice is empty, Confidence is nil,
// Probabilities is nil, or Choice is not a key of Probabilities.
func (r *Response) Choice(id string) (
	choice string,
	confidence float64,
	probs map[string]float64,
	err error,
) {
	a, ok := r.Answers[id]
	if !ok {
		return "", 0, nil, &ResponseError{
			Message: fmt.Sprintf("answer %q missing", id),
		}
	}
	if a.Type != "choice" {
		return "", 0, nil, &ResponseError{Message: fmt.Sprintf(
			"answer %q has type %q, want \"choice\"", id, a.Type,
		)}
	}
	if a.Choice == "" {
		return "", 0, nil, &ResponseError{
			Message: fmt.Sprintf("answer %q has no choice", id),
		}
	}
	if a.Confidence == nil {
		return "", 0, nil, &ResponseError{
			Message: fmt.Sprintf("answer %q has no confidence", id),
		}
	}
	if a.Probabilities == nil {
		return "", 0, nil, &ResponseError{
			Message: fmt.Sprintf("answer %q has no probabilities", id),
		}
	}
	if _, ok := a.Probabilities[a.Choice]; !ok {
		return "", 0, nil, &ResponseError{Message: fmt.Sprintf(
			"answer %q chose %q, absent from probabilities",
			id, a.Choice,
		)}
	}

	return a.Choice, *a.Confidence, a.Probabilities, nil
}

// Client sends System One requests. Construct with New; hc is
// injected.
type Client struct {
	hc      *http.Client
	baseURL string
	apiKey  string
	model   string
	sleep   func(ctx context.Context, d time.Duration) error
}

// New returns a Client that posts to baseURL with apiKey, defaulting
// requests without a model to model.
func New(hc *http.Client, baseURL, apiKey, model string) *Client {
	return &Client{
		hc:      hc,
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		sleep:   wait,
	}
}

// wait sleeps for d unless ctx is cancelled first.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Ask marshals req (filling an empty Model from the client default)
// and calls AskRaw.
func (c *Client) Ask(
	ctx context.Context, req Request,
) (*Response, error) {
	if req.Model == "" {
		req.Model = c.model
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	return c.AskRaw(ctx, body)
}

// AskRaw POSTs body verbatim to {base}/v1/systemone and decodes the
// response. Status 429 and any 5xx are retried up to three attempts
// in total.
func (c *Client) AskRaw(
	ctx context.Context, body []byte,
) (*Response, error) {
	var lastErr error

	for attempt := range len(retryWaits) + 1 {
		if attempt > 0 {
			d := retryWaits[attempt-1]
			if ra, ok := lastErr.(*retryable); ok && ra.hasAfter {
				d = ra.after
			}
			if err := c.sleep(ctx, d); err != nil {
				return nil, &NetworkError{Err: err}
			}
		}

		resp, err := c.attempt(ctx, body)
		if err == nil {
			return resp, nil
		}
		if r, ok := err.(*retryable); ok {
			lastErr = r

			continue
		}

		return nil, err
	}

	return nil, lastErr.(*retryable).final()
}

// retryable wraps a response worth retrying, carrying any Retry-After
// the server sent and the error to report once retries run out.
type retryable struct {
	status   int
	message  string
	after    time.Duration
	hasAfter bool
}

// Error implements error so retryable can travel as one internally.
func (r *retryable) Error() string { return r.final().Error() }

// final returns the error to report after the last attempt.
func (r *retryable) final() error {
	if r.status == http.StatusTooManyRequests {
		return &RateLimitError{Status: r.status}
	}

	return &ServerError{Status: r.status, Message: r.message}
}

// attempt performs one HTTP request. A retryable status is returned as
// a *retryable; every other failure is returned as its final error.
func (c *Client) attempt(
	ctx context.Context, body []byte,
) (*Response, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+Path, bytes.NewReader(body),
	)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer drainClose(resp.Body)

	if resp.StatusCode == http.StatusOK {
		out, err := decodeResponse(resp.Body)
		if err != nil {
			return nil, c.bodyError(ctx, err)
		}

		return out, nil
	}

	msg := c.redact(readErrBody(resp.Body))

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &AuthError{Status: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		r := &retryable{status: resp.StatusCode, message: msg}
		r.after, r.hasAfter = retryAfter(resp.Header)

		return nil, r
	default:
		return nil, &RequestError{
			Status: resp.StatusCode, Message: msg,
		}
	}
}

// drainClose discards up to drainLimit bytes of whatever remains in
// body and then closes it, so the connection can be reused.
func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimit))
	_ = body.Close()
}

// decodeResponse reads exactly one System One response object from r.
// The body must be a lone JSON object whose "answers" member is a
// non-null object, followed by EOF. Every content violation is
// returned as a *ResponseError; a read failure is returned unwrapped
// so the caller can classify it.
func decodeResponse(r io.Reader) (*Response, error) {
	dec := json.NewDecoder(r)

	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if isReadFailure(err) {
			return nil, err
		}

		return nil, &ResponseError{
			Message: "decode body: " + err.Error(),
		}
	}

	if err := requireEOF(dec); err != nil {
		return nil, err
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, &ResponseError{
			Message: "body is not a JSON object",
		}
	}
	if members == nil {
		return nil, &ResponseError{Message: "body is null"}
	}

	answers, ok := members["answers"]
	if !ok {
		return nil, &ResponseError{Message: `body has no "answers"`}
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &ResponseError{
			Message: "decode body: " + err.Error(),
		}
	}
	if out.Answers == nil {
		return nil, &ResponseError{Message: `"answers" is ` +
			answersShape(answers)}
	}

	return &out, nil
}

// answersShape names why an "answers" member yielded no map, for the
// error message.
func answersShape(answers json.RawMessage) string {
	if string(answers) == "null" {
		return "null"
	}

	return "not an object"
}

// requireEOF reports a *ResponseError unless dec holds nothing but
// trailing whitespace.
func requireEOF(dec *json.Decoder) error {
	var rest json.RawMessage
	err := dec.Decode(&rest)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil && isReadFailure(err) {
		return err
	}

	return &ResponseError{
		Message: "body holds more than one JSON value",
	}
}

// isReadFailure reports whether err came from the transport or a
// cancelled context rather than from the body's content.
func isReadFailure(err error) bool {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error

	return errors.As(err, &netErr)
}

// bodyError classifies a failure raised while reading a 200 body. A
// transport failure, timeout, or cancelled context is a NetworkError
// so it keeps its exit code; invalid content stays a ResponseError.
func (c *Client) bodyError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &NetworkError{Err: ctxErr}
	}
	if isReadFailure(err) {
		return &NetworkError{Err: err}
	}

	var respErr *ResponseError
	if errors.As(err, &respErr) {
		respErr.Message = c.redact(respErr.Message)

		return respErr
	}

	return &ResponseError{Message: c.redact(err.Error())}
}

// redact replaces every occurrence of the configured API key in s
// with a placeholder, so an upstream diagnostic that echoes the
// credential cannot reach an error value, a log record, or a stream.
func (c *Client) redact(s string) string {
	if c.apiKey == "" {
		return s
	}

	return strings.ReplaceAll(s, c.apiKey, "[redacted]")
}

// readErrBody reads at most maxErrBody bytes of an error body.
func readErrBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, maxErrBody))
	if err != nil {
		return ""
	}

	return string(b)
}

// retryAfter parses an integer Retry-After header in seconds.
func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}

	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0, false
	}

	return time.Duration(secs) * time.Second, true
}
