package acp

import (
	"encoding/json"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// DecodeInitializeRequest selects the supported version before interpreting
// any version-specific fields and returns the client's usable capabilities.
func DecodeInitializeRequest(params json.RawMessage) (ClientCapabilities, error) {
	var req struct {
		ProtocolVersion json.RawMessage `json:"protocolVersion,case:strict"`
		Info            json.RawMessage `json:"info,case:strict"`
		Capabilities    json.RawMessage `json:"capabilities,case:strict"`
	}
	if err := jsonrpc.DecodeParams(params, &req); err != nil {
		return ClientCapabilities{}, err
	}
	if req.ProtocolVersion == nil || string(req.ProtocolVersion) == "null" {
		return ClientCapabilities{}, ErrInvalidParams("protocolVersion is required")
	}
	version, err := jsonrpc.ParseInteger(req.ProtocolVersion, 32)
	if err != nil || version < 0 || version > 65535 {
		return ClientCapabilities{}, ErrInvalidParams("protocolVersion must be an integer between 0 and 65535")
	}
	requested := int(version)
	if requested < ProtocolVersion {
		return ClientCapabilities{}, ErrUnsupportedProtocolVersion(requested)
	}

	// acplause does not use client info. Require the v2 shape from v2
	// clients, and allow newer clients to send a shape we cannot read.
	if requested == ProtocolVersion {
		if err := validateInitializeInfo(req.Info); err != nil {
			return ClientCapabilities{}, err
		}
	}
	var capabilities ClientCapabilities
	// Capabilities default to unsupported when absent or unreadable.
	_ = json.Unmarshal(req.Capabilities, &capabilities)
	return capabilities, nil
}

func validateInitializeInfo(raw json.RawMessage) error {
	var info struct {
		Name    *string `json:"name,case:strict"`
		Version *string `json:"version,case:strict"`
	}
	if err := jsonrpc.DecodeParams(raw, &info); err != nil {
		return err
	}
	if info.Name == nil || info.Version == nil {
		return ErrInvalidParams("info.name and info.version are required")
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
		Elicitation json.RawMessage `json:"elicitation,case:strict"`
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
		Form json.RawMessage `json:"form,case:strict"`
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
