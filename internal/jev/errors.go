package jev

import "fmt"

// AuthError reports that the API rejected the credentials (401).
type AuthError struct {
	Status int
}

// Error implements error.
func (e *AuthError) Error() string {
	return fmt.Sprintf("authentication failed (status %d)", e.Status)
}

// RateLimitError reports a 429 that survived the client's retries.
type RateLimitError struct {
	Status int
}

// Error implements error.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (status %d)", e.Status)
}

// ServerError reports a 5xx that survived the client's retries.
type ServerError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *ServerError) Error() string {
	return fmt.Sprintf(
		"upstream error (status %d): %s", e.Status, e.Message,
	)
}

// RequestError reports a 4xx other than 401 or 429: the request was
// rejected.
type RequestError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *RequestError) Error() string {
	return fmt.Sprintf(
		"request rejected (status %d): %s", e.Status, e.Message,
	)
}

// ResponseError reports a 200 response that violates the documented
// answer contract.
type ResponseError struct {
	Message string
}

// Error implements error.
func (e *ResponseError) Error() string {
	return "invalid response: " + e.Message
}

// NetworkError reports a transport failure, timeout, or cancelled
// context.
type NetworkError struct {
	Err error
}

// Error implements error.
func (e *NetworkError) Error() string {
	return "network failure: " + e.Err.Error()
}

// Unwrap returns the underlying transport or context error.
func (e *NetworkError) Unwrap() error { return e.Err }
