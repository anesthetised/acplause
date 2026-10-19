// Package acp holds the Agent Client Protocol v2 wire types acplause uses,
// written from the pinned schema in testdata/schema/v2. Only the subset the
// agent sends or reads is modelled; unknown incoming fields are ignored.
package acp

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// ProtocolVersion is the only ACP version spoken.
const ProtocolVersion = 2

// Method names.
const (
	MethodInitialize       = "initialize"
	MethodSessionNew       = "session/new"
	MethodSessionPrompt    = "session/prompt"
	MethodSessionCancel    = "session/cancel"
	MethodSessionList      = "session/list"
	MethodSessionResume    = "session/resume"
	MethodSessionClose     = "session/close"
	MethodSessionSetConfig = "session/set_config_option"
	MethodSessionUpdate    = "session/update"

	MethodSessionRequestPermission = "session/request_permission"
)

// ACP error codes beyond the JSON-RPC standard set.
const (
	CodeAuthRequired               = -32000
	CodeResourceNotFound           = -32002
	CodeUnsupportedProtocolVersion = -32022
	CodeRequestCancelled           = -32800
)

// ErrRequestCancelled answers a request cancelled before it took effect.
func ErrRequestCancelled() *jsonrpc.Error {
	return &jsonrpc.Error{Code: CodeRequestCancelled, Message: "Request cancelled"}
}

// ErrSessionNotFound answers a request naming an unknown session.
func ErrSessionNotFound(id string) *jsonrpc.Error {
	return jsonrpc.NewError(CodeResourceNotFound, "session not found: %s", id)
}

// ErrUnsupportedProtocolVersion rejects initialize for a version below 2.
func ErrUnsupportedProtocolVersion(requested int) *jsonrpc.Error {
	return &jsonrpc.Error{
		Code:    CodeUnsupportedProtocolVersion,
		Message: "Unsupported protocol version",
		Data:    map[string]any{"requested": requested, "supported": []int{ProtocolVersion}},
	}
}

// ErrInvalidParams reports a semantically invalid parameter.
func ErrInvalidParams(format string, args ...any) *jsonrpc.Error {
	return jsonrpc.NewError(jsonrpc.CodeInvalidParams, format, args...)
}

// Supported is the empty-object marker the schema uses for "supported".
type Supported struct{}

func requireSessionID(id string) error {
	if id == "" {
		return errors.New("sessionId is required")
	}
	return nil
}

func requireAbsolute(field, path string) error {
	if path == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be an absolute path: %q", field, path)
	}
	return nil
}

func requireAbsoluteAll(field string, paths []string) error {
	for _, p := range paths {
		if err := requireAbsolute(field, p); err != nil {
			return err
		}
	}
	return nil
}
