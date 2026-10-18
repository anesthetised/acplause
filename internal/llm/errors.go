package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ErrorKind classifies failures so callers can react without parsing text.
type ErrorKind string

// Error kinds.
const (
	KindAuth            ErrorKind = "auth"             // 401, 403
	KindPaymentRequired ErrorKind = "payment_required" // 402: out of credits
	KindBadRequest      ErrorKind = "bad_request"      // 400, 413, 422
	KindNotFound        ErrorKind = "not_found"        // 404: unknown model or URL
	KindTimeout         ErrorKind = "timeout"          // 408
	KindRateLimited     ErrorKind = "rate_limited"     // 429
	KindServer          ErrorKind = "server"           // 5xx
	KindUnreachable     ErrorKind = "unreachable"      // transport failure
	KindProvider        ErrorKind = "provider"         // error reported inside the stream
	KindTruncated       ErrorKind = "truncated"        // stream ended without [DONE]
	KindProtocol        ErrorKind = "protocol"         // unparseable response
	KindUnknown         ErrorKind = "unknown"
)

// Error is a classified API failure.
type Error struct {
	Kind    ErrorKind
	Status  int // HTTP status; 0 when there is none
	Message string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("llm: %s (HTTP %d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("llm: %s: %s", e.Kind, e.Message)
}

// Retryable reports whether repeating the request may succeed.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindTimeout, KindRateLimited, KindServer, KindUnreachable:
		return true
	}
	return false
}

func kindForStatus(status int) ErrorKind {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusPaymentRequired:
		return KindPaymentRequired
	case status == http.StatusNotFound:
		return KindNotFound
	case status == http.StatusRequestTimeout:
		return KindTimeout
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status >= 500:
		return KindServer
	case status >= 400:
		return KindBadRequest
	}
	return KindUnknown
}

// apiError is the error object OpenRouter puts in error bodies and, mid-
// stream, at the top level of a chunk.
type apiError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

// statusError builds an Error from a non-2xx response body.
func statusError(status int, body []byte) *Error {
	var envelope struct {
		Error *apiError `json:"error"`
	}
	message := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &Error{Kind: kindForStatus(status), Status: status, Message: message}
}

func (e *apiError) toError() *Error {
	message := e.Message
	if message == "" {
		message = "provider error"
	}
	if code := strings.Trim(string(e.Code), `"`); code != "" && code != "null" {
		message = code + ": " + message
	}
	return &Error{Kind: KindProvider, Message: message}
}
