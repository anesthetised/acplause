package acp

import (
	"encoding/json"
	"errors"
)

// NewSessionRequest is the params of session/new. MCP servers are accepted
// and ignored.
type NewSessionRequest struct {
	Cwd                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories"`
	MCPServers            []json.RawMessage `json:"mcpServers"`
}

// Validate implements jsonrpc's params validation.
func (r NewSessionRequest) Validate() error {
	return errors.Join(
		requireAbsolute("cwd", r.Cwd),
		requireAbsoluteAll("additionalDirectories", r.AdditionalDirectories),
	)
}

// NewSessionResponse is the result of session/new.
type NewSessionResponse struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

// ResumeSessionRequest is the params of session/resume.
type ResumeSessionRequest struct {
	SessionID             string            `json:"sessionId"`
	Cwd                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories"`
	MCPServers            []json.RawMessage `json:"mcpServers"`
	ReplayFrom            *ReplayFrom       `json:"replayFrom"`
}

// Validate implements jsonrpc's params validation.
func (r ResumeSessionRequest) Validate() error {
	var replay error
	if r.ReplayFrom != nil && r.ReplayFrom.Type == "" {
		replay = errors.New("replayFrom.type is required")
	}
	return errors.Join(
		requireSessionID(r.SessionID),
		requireAbsolute("cwd", r.Cwd),
		requireAbsoluteAll("additionalDirectories", r.AdditionalDirectories),
		replay,
	)
}

// ReplayFrom is a history replay cursor.
type ReplayFrom struct {
	Type string `json:"type"`
}

// ReplayFromStart replays all retained history.
const ReplayFromStart = "start"

// ResumeSessionResponse is the result of session/resume.
type ResumeSessionResponse struct {
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

// CloseSessionRequest is the params of session/close.
type CloseSessionRequest struct {
	SessionID string `json:"sessionId"`
}

// Validate implements jsonrpc's params validation.
func (r CloseSessionRequest) Validate() error { return requireSessionID(r.SessionID) }

// CloseSessionResponse is the empty result of session/close.
type CloseSessionResponse struct{}

// DeleteSessionRequest is the params of session/delete.
type DeleteSessionRequest struct {
	SessionID string `json:"sessionId"`
}

// Validate implements jsonrpc's params validation.
func (r DeleteSessionRequest) Validate() error { return requireSessionID(r.SessionID) }

// DeleteSessionResponse is the empty result of session/delete.
type DeleteSessionResponse struct{}

// CancelSessionNotification is the params of session/cancel.
type CancelSessionNotification struct {
	SessionID string `json:"sessionId"`
}

// Validate implements jsonrpc's params validation.
func (n CancelSessionNotification) Validate() error { return requireSessionID(n.SessionID) }

// ListSessionsRequest is the params of session/list. Both fields are
// optional; null and omission are the same.
type ListSessionsRequest struct {
	Cwd    string `json:"cwd"`
	Cursor string `json:"cursor"`
}

// Validate implements jsonrpc's params validation.
func (r ListSessionsRequest) Validate() error {
	if r.Cwd == "" {
		return nil
	}
	return requireAbsolute("cwd", r.Cwd)
}

// ListSessionsResponse is one page of session/list results. NextCursor is
// omitted on the last page.
type ListSessionsResponse struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// SessionInfo describes a listed session.
type SessionInfo struct {
	SessionID             string   `json:"sessionId"`
	Cwd                   string   `json:"cwd"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	Title                 string   `json:"title,omitempty"`
	UpdatedAt             string   `json:"updatedAt,omitempty"`
}

// SetConfigOptionRequest is the params of session/set_config_option.
type SetConfigOptionRequest struct {
	SessionID string          `json:"sessionId"`
	ConfigID  string          `json:"configId"`
	Type      string          `json:"type"`
	Value     json.RawMessage `json:"value"`
}

// Config value shapes.
const (
	ConfigValueID      = "id"
	ConfigValueBoolean = "boolean"
)

// Validate implements jsonrpc's params validation.
func (r SetConfigOptionRequest) Validate() error {
	var errs []error
	if r.ConfigID == "" {
		errs = append(errs, errors.New("configId is required"))
	}
	if r.Type == "" {
		errs = append(errs, errors.New("type is required"))
	}
	if r.Value == nil {
		errs = append(errs, errors.New("value is required"))
	}
	return errors.Join(append(errs, requireSessionID(r.SessionID))...)
}

// SetConfigOptionResponse carries the complete option list after a change.
type SetConfigOptionResponse struct {
	ConfigOptions []ConfigOption `json:"configOptions"`
}

// ConfigOption is a session setting. acplause only uses select options.
type ConfigOption struct {
	ConfigID     string              `json:"configId"`
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	Category     string              `json:"category,omitempty"`
	Type         string              `json:"type"`
	CurrentValue string              `json:"currentValue"`
	Options      []ConfigOptionValue `json:"options"`
}

// ConfigOption types and categories.
const (
	ConfigTypeSelect = "select"

	CategoryModel        = "model"
	CategoryThoughtLevel = "thought_level"
)

// ConfigOptionValue is one choice of a select option.
type ConfigOptionValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}
