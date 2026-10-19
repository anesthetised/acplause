package acp

import (
	"errors"
	"fmt"
)

// InitializeRequest is the client's initialize params. Client capabilities
// are not used: acplause calls nothing on the client.
type InitializeRequest struct {
	ProtocolVersion *int            `json:"protocolVersion"`
	Info            *Implementation `json:"info"`
}

// Validate implements jsonrpc's params validation.
func (r InitializeRequest) Validate() error {
	switch {
	case r.ProtocolVersion == nil:
		return errors.New("protocolVersion is required")
	case *r.ProtocolVersion < 0 || *r.ProtocolVersion > 65535:
		return fmt.Errorf("protocolVersion out of range: %d", *r.ProtocolVersion)
	case r.Info == nil:
		return errors.New("info is required")
	}
	return nil
}

// Implementation identifies a client or agent.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// InitializeResponse is the agent's initialize result.
type InitializeResponse struct {
	ProtocolVersion int               `json:"protocolVersion"`
	Info            Implementation    `json:"info"`
	Capabilities    AgentCapabilities `json:"capabilities"`
}

// AgentCapabilities advertises the agent's method surfaces.
type AgentCapabilities struct {
	// Session, when present, promises the whole baseline session surface:
	// new, list, resume, close, prompt, cancel and update.
	Session *SessionCapabilities `json:"session,omitempty"`
}

// SessionCapabilities lists optional session features.
type SessionCapabilities struct {
	Prompt                *PromptCapabilities `json:"prompt,omitempty"`
	AdditionalDirectories *Supported          `json:"additionalDirectories,omitempty"`
}

// PromptCapabilities lists prompt content beyond the text and resource_link
// baseline. acplause adds embedded resources only.
type PromptCapabilities struct {
	EmbeddedContext *Supported `json:"embeddedContext,omitempty"`
}
