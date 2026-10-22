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

// ElicitationSchema describes the required fields acplause requests.
type ElicitationSchema struct {
	Type       string                         `json:"type"`
	Properties map[string]ElicitationProperty `json:"properties"`
	Required   []string                       `json:"required"`
}

// ElicitationProperty is a scalar answer or an array of string choices.
type ElicitationProperty struct {
	Type        string                `json:"type"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Enum        []string              `json:"enum,omitempty"`
	Items       *ElicitationEnumItems `json:"items,omitempty"`
	MaxItems    int                   `json:"maxItems,omitempty"`
}

// ElicitationEnumItems limits multiselect answers to offered string values.
type ElicitationEnumItems struct {
	Type string   `json:"type"`
	Enum []string `json:"enum"`
}

// ElicitationResponse retains answer JSON until the requested fields validate.
// Unknown actions are readable but never accepted as answers.
type ElicitationResponse struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content"`
}
