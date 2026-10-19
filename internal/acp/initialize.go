package acp

import (
	"encoding/json"
	"errors"
	"fmt"
)

// InitializeRequest is the client's initialize params.
type InitializeRequest struct {
	Capabilities    ClientCapabilities `json:"capabilities"`
	ProtocolVersion *int               `json:"protocolVersion"`
	Info            *Implementation    `json:"info"`
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
	MCP                   *MCPCapabilities    `json:"mcp,omitempty"`
	Delete                *Supported          `json:"delete,omitempty"`
	Prompt                *PromptCapabilities `json:"prompt,omitempty"`
	AdditionalDirectories *Supported          `json:"additionalDirectories,omitempty"`
}

// PromptCapabilities lists prompt content beyond the text and resource_link
// baseline.
type PromptCapabilities struct {
	EmbeddedContext *Supported `json:"embeddedContext,omitempty"`
	Image           *Supported `json:"image,omitempty"`
}

// ClientCapabilities contains the client surfaces acplause uses.
type ClientCapabilities struct {
	Elicitation *ElicitationCapabilities `json:"elicitation"`
}

// UnmarshalJSON applies the pinned schema's default-on-error rules to both
// the capabilities group and its optional elicitation capability.
func (c *ClientCapabilities) UnmarshalJSON(data []byte) error {
	var raw struct {
		Elicitation json.RawMessage `json:"elicitation"`
	}
	*c = ClientCapabilities{}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	if json.Unmarshal(raw.Elicitation, &c.Elicitation) != nil {
		c.Elicitation = nil
	}
	return nil
}

// ElicitationCapabilities negotiates direct agent form requests.
type ElicitationCapabilities struct {
	Form *Supported `json:"form"`
}

// UnmarshalJSON treats a malformed optional form marker as unsupported,
// rather than failing the initialization of an otherwise usable client.
func (c *ElicitationCapabilities) UnmarshalJSON(data []byte) error {
	var raw struct {
		Form json.RawMessage `json:"form"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = ElicitationCapabilities{}
	if json.Unmarshal(raw.Form, &c.Form) != nil {
		c.Form = nil
	}
	return nil
}
