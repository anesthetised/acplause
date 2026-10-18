// Package jsonrpc implements JSON-RPC 2.0 over newline-delimited JSON, as
// used by the ACP stdio transport.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the only protocol version spoken.
const Version = "2.0"

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error is a JSON-RPC error object. Handlers return it to choose the code
// sent to the peer; any other error becomes an Internal error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// NewError builds an Error with a formatted message.
func NewError(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// AsError converts any error into a wire error.
func AsError(err error) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	return &Error{Code: CodeInternalError, Message: err.Error()}
}

// ID is a request id held as its raw JSON text, so numbers and strings are
// echoed back exactly as received and ids can key maps. The zero value
// marshals as null.
type ID string

// NullID is the id used when a request's own id cannot be determined.
const NullID ID = "null"

// MarshalJSON implements json.Marshaler.
func (id ID) MarshalJSON() ([]byte, error) {
	if id == "" {
		return []byte("null"), nil
	}
	return []byte(id), nil
}

// UnmarshalJSON implements json.Unmarshaler. Only strings, numbers and null
// are valid ids.
func (id *ID) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch v.(type) {
	case string, float64, nil:
	default:
		return errors.New("id must be a string, a number or null")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return err
	}
	*id = ID(compact.String())
	return nil
}

// inbound is any message read from the peer.
type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func (m *inbound) isResponse() bool { return m.Method == "" && (m.Result != nil || m.Error != nil) }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func newResponse(id ID, result any, err error) *response {
	if err != nil {
		return errorResponse(id, AsError(err))
	}
	raw, err := Marshal(result)
	if err != nil {
		return errorResponse(id, NewError(CodeInternalError, "encode result: %v", err))
	}
	return &response{JSONRPC: Version, ID: id, Result: raw}
}

func errorResponse(id ID, err *Error) *response {
	return &response{JSONRPC: Version, ID: id, Error: err}
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Marshal encodes v like json.Marshal but leaves <, > and & unescaped: the
// output is read by programs, and `<` in code is far more common than in
// markup. MarshalJSON methods of wire types should use it too, because the
// connection does not unescape what they return.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// DecodeParams unmarshals params into v and runs v's Validate method when it
// has one. Failures are reported as Invalid params. Absent and null params
// decode as an empty object; unknown fields are ignored.
func DecodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage("{}")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return NewError(CodeInvalidParams, "invalid params: %v", err)
	}
	if val, ok := v.(interface{ Validate() error }); ok {
		if err := val.Validate(); err != nil {
			return NewError(CodeInvalidParams, "invalid params: %v", err)
		}
	}
	return nil
}
