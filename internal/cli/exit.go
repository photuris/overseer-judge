package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"overseer-judge/internal/config"
	"overseer-judge/internal/jev"
)

// Exit codes, per PLAN.md. 4 and 9 are unused: there are no resources
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
		usage     *usageError
		auth      *jev.AuthError
		request   *jev.RequestError
		rateLimit *jev.RateLimitError
		server    *jev.ServerError
		response  *jev.ResponseError
		network   *jev.NetworkError
	)

	switch {
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
// returns the exit code for err.
func reportError(stderr io.Writer, err error) int {
	body, code := classify(err)
	if encErr := json.NewEncoder(stderr).Encode(
		errorRecord{Error: body},
	); encErr != nil {
		return exitUnknown
	}

	return code
}
