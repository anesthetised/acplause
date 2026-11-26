// Package jsonrpc implements JSON-RPC 2.0 over newline-delimited JSON, as
// used by the ACP stdio transport.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// ID is an RPC id held as raw JSON text so replies can echo the original
// spelling. The zero value marshals as null.
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

// UnmarshalJSON implements json.Unmarshaler. Null is reserved for error
// responses whose request ID is unknown.
func (id *ID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*id = NullID
		return nil
	}
	if !json.Valid(b) {
		return errors.New("invalid JSON id")
	}
	wire, _ := requestID(b)
	if wire == "" {
		return errors.New("id must be a string or an integer")
	}
	*id = wire
	return nil
}

// requestID takes a JSON token and returns its original spelling and a key
// for matching equivalent JSON values. An empty pair means no valid ID.
func requestID(raw json.RawMessage) (wire, key ID) {
	if len(raw) == 0 {
		return "", ""
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", ""
		}
		return ID(raw), ID(strconv.Quote(value))
	}
	if key = integerKey(string(raw)); key == "" {
		return "", ""
	}
	return ID(raw), key
}

// integerKey normalizes integral numbers without rounding. The old decoder
// accepted finite float64 values, which have at most 309 integer digits.
// Keep that width for compatibility and check it before expanding exponents.
func integerKey(number string) ID {
	const maxDigits = 309
	if number == "" || (number[0] != '-' && (number[0] < '0' || number[0] > '9')) {
		return ""
	}
	mantissa, exponent := number, "0"
	if i := strings.IndexAny(number, "eE"); i >= 0 {
		mantissa, exponent = number[:i], number[i+1:]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	unsigned, negative := strings.CutPrefix(whole, "-")
	significant := strings.TrimRight(unsigned+fraction, "0")
	if significant == "" {
		return "0"
	}
	scale := int64(len(unsigned) - len(significant))
	digits := strings.TrimLeft(significant, "0")
	exp, err := strconv.ParseInt(exponent, 10, 64)
	if err != nil || exp < -scale || exp > int64(maxDigits-len(digits))-scale {
		return ""
	}
	if negative {
		digits = "-" + digits
	}
	return ID(digits + strings.Repeat("0", int(exp+scale)))
}

// ParseInteger reads an integral JSON number exactly and checks the signed
// range for bitSize. Decimal and exponent spellings are accepted.
func ParseInteger(raw json.RawMessage, bitSize int) (int64, error) {
	if !json.Valid(raw) {
		return 0, errors.New("expected an integer")
	}
	return strconv.ParseInt(string(integerKey(string(bytes.TrimSpace(raw)))), 10, bitSize)
}

// inbound is any message read from the peer.
type inbound struct {
	JSONRPC json.RawMessage `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  json.RawMessage `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`

	peerError *Error
}

func (m *inbound) isResponse() bool { return m.Method == nil && (m.Result != nil || m.Error != nil) }

// failed treats "error": null as absent: some peers send it next to a result.
func (m *inbound) failed() bool { return m.Error != nil && string(m.Error) != "null" }

// parseResponse checks the envelope and decodes any error before a peer can
// consume a pending call. Delivery separately checks the response ID.
func (m *inbound) parseResponse() bool {
	if m.Result != nil {
		return !m.failed()
	}
	var e struct {
		Code    json.RawMessage `json:"code,case:strict"`
		Message *string         `json:"message,case:strict"`
		Data    any             `json:"data,case:strict"`
	}
	if !m.failed() || json.Unmarshal(m.Error, &e) != nil || e.Message == nil {
		return false
	}
	code, err := ParseInteger(e.Code, 32)
	if err != nil {
		return false
	}
	m.peerError = &Error{Code: int(code), Message: *e.Message, Data: e.Data}
	return true
}

type resultWithWriteHooks struct {
	result      any
	beforeWrite func()
	afterWrite  func(error)
}

type resultWithEnqueueHook struct {
	result  any
	enqueue func()
}

// OnEnqueue runs a nonblocking callback after a successful synchronous
// response enters the outgoing queue, before the next request is dispatched.
func OnEnqueue(result any, enqueue func()) any {
	return resultWithEnqueueHook{result, enqueue}
}

// OnWrite attaches nonblocking callbacks to a successful response. The writer
// brackets the frame's own Write, treating a short write or sealed connection
// as an unsuccessful delivery. Callbacks must not take locks or perform I/O.
func OnWrite(result any, before func(), after func(error)) any {
	return resultWithWriteHooks{result, before, after}
}

type response struct {
	afterEnqueue func()
	beforeWrite  func()
	afterWrite   func(error)
	JSONRPC      string          `json:"jsonrpc"`
	ID           ID              `json:"id"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *Error          `json:"error,omitempty"`
}

func newResponse(id ID, result any, err error) *response {
	if err != nil {
		return errorResponse(id, AsError(err))
	}
	var beforeWrite func()
	var afterWrite func(error)
	var afterEnqueue func()
	switch r := result.(type) {
	case resultWithWriteHooks:
		result, beforeWrite, afterWrite = r.result, r.beforeWrite, r.afterWrite
	case resultWithEnqueueHook:
		result, afterEnqueue = r.result, r.enqueue
	}
	raw, err := Marshal(result)
	if err != nil {
		return errorResponse(id, NewError(CodeInternalError, "encode result: %v", err))
	}
	return &response{JSONRPC: Version, ID: id, Result: raw, beforeWrite: beforeWrite, afterWrite: afterWrite, afterEnqueue: afterEnqueue}
}

func errorResponse(id ID, err *Error) *response {
	return &response{JSONRPC: Version, ID: id, Error: err}
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      ID     `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
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
