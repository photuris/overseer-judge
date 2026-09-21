package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/photuris/overseer-judge/internal/config"
	"github.com/photuris/overseer-judge/internal/jev"
)

// Exit codes, per README.md. 4 and 9 are unused: there are no resources
// to not-find and no bulk writes.
const (
	exitOK        = 0
	exitUnknown   = 1
	exitUsage     = 2
	exitAuth      = 3
	exitRequest   = 5
	exitRateLimit = 6
	exitUpstream  = 7
	exitNetwork   = 8
	exitInterrupt = 130
)

// usageError is a local failure: bad flags, a missing argument, or
// input that is not a valid Jev request.
type usageError struct {
	Message string
}

// Error implements error.
func (e *usageError) Error() string { return e.Message }

// interruptedError reports that the root context was cancelled. Run
// substitutes it for whatever error a verb returned first, so an
// interrupt is classified the same way on every path.
type interruptedError struct {
	Err error
}

// Error implements error.
func (e *interruptedError) Error() string {
	return "interrupted: " + e.Err.Error()
}

// Unwrap returns the context error that cancelled the command.
func (e *interruptedError) Unwrap() error { return e.Err }

// usagef returns a usageError with a formatted message.
func usagef(format string, args ...any) error {
	return &usageError{Message: fmt.Sprintf(format, args...)}
}

// errorRecord is the JSON object written as the last stderr line.
type errorRecord struct {
	Error errorBody `json:"error"`
}

// errorBody describes one failure.
type errorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

// classify maps err onto its error record and exit code. The
// context.Canceled case precedes the NetworkError one because a
// cancelled request surfaces as a NetworkError wrapping it.
func classify(err error) (errorBody, int) {
	body := errorBody{Type: "unknown", Message: err.Error()}

	var (
		interrupt *interruptedError
		usage     *usageError
		auth      *jev.AuthError
		request   *jev.RequestError
		rateLimit *jev.RateLimitError
		server    *jev.ServerError
		response  *jev.ResponseError
		network   *jev.NetworkError
	)

	switch {
	case errors.As(err, &interrupt):
		body.Type = "interrupted"

		return body, exitInterrupt
	case errors.As(err, &usage):
		body.Type = "usage"

		return body, exitUsage
	case errors.Is(err, config.ErrNoKey):
		body.Type = "auth"

		return body, exitAuth
	case errors.As(err, &auth):
		body.Type, body.Status = "auth", auth.Status

		return body, exitAuth
	case errors.As(err, &request):
		body.Type, body.Status = "request", request.Status

		return body, exitRequest
	case errors.As(err, &rateLimit):
		body.Type, body.Status = "rate_limit", rateLimit.Status

		return body, exitRateLimit
	case errors.As(err, &server):
		body.Type, body.Status = "server", server.Status

		return body, exitUpstream
	case errors.As(err, &response):
		body.Type = "response"

		return body, exitUpstream
	case errors.Is(err, context.Canceled):
		body.Type = "interrupted"

		return body, exitInterrupt
	case errors.As(err, &network):
		body.Type = "network"

		return body, exitNetwork
	}

	return body, exitUnknown
}

// codeFor returns the exit code for err.
func codeFor(err error) int {
	_, code := classify(err)

	return code
}

// reportError writes the error record as the last stderr line and
// returns the exit code for err. redact strips the resolved API key
// from the message, so an upstream diagnostic that echoes the
// credential cannot escape through any error path.
func reportError(
	stderr io.Writer, err error, redact func(string) string,
) int {
	body, code := classify(err)
	body.Message = redact(body.Message)

	if encErr := json.NewEncoder(stderr).Encode(
		errorRecord{Error: body},
	); encErr != nil {
		return exitUnknown
	}

	return code
}
