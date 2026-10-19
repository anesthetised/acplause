package acp

import "encoding/json"

// MethodElicitationCreate asks the client for structured input.
const MethodElicitationCreate = "elicitation/create"

// ElicitationRequest is the session-scoped form subset of elicitation/create.
// Scope is flattened on the wire, rather than nested under a scope object.
type ElicitationRequest struct {
	Mode            string            `json:"mode"`
	SessionID       string            `json:"sessionId"`
	ToolCallID      string            `json:"toolCallId"`
	Message         string            `json:"message"`
	RequestedSchema ElicitationSchema `json:"requestedSchema"`
}

// ElicitationSchema describes the required string fields acplause requests.
type ElicitationSchema struct {
	Type       string                               `json:"type"`
	Properties map[string]ElicitationStringProperty `json:"properties"`
	Required   []string                             `json:"required"`
}

// ElicitationStringProperty is a free-text or single-select form field.
type ElicitationStringProperty struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// ElicitationResponse retains answer JSON until the requested fields validate.
// Unknown actions are readable but never accepted as answers.
type ElicitationResponse struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content"`
}
