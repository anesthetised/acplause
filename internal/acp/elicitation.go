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

// ElicitationSchema describes the form fields acplause requests. Required
// lists only mandatory fields; null and omitted are equivalent on the wire.
type ElicitationSchema struct {
	Type        string                         `json:"type"`
	Title       string                         `json:"title,omitempty"`
	Description string                         `json:"description,omitempty"`
	Properties  map[string]ElicitationProperty `json:"properties"`
	Required    []string                       `json:"required,omitempty"`
}

// ElicitationProperty is a scalar answer or an array of string choices. The
// constraint fields hold exact JSON tokens or pointers, because zero values
// (a 0 bound, a false default, an empty selection) are meaningful and must
// not be omitted.
type ElicitationProperty struct {
	Type        string                `json:"type"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Enum        []string              `json:"enum,omitempty"`
	OneOf       []ElicitationOption   `json:"oneOf,omitempty"`
	Items       *ElicitationEnumItems `json:"items,omitempty"`
	MinLength   *int                  `json:"minLength,omitempty"`
	MaxLength   *int                  `json:"maxLength,omitempty"`
	Format      string                `json:"format,omitempty"`
	Minimum     json.RawMessage       `json:"minimum,omitempty"`
	Maximum     json.RawMessage       `json:"maximum,omitempty"`
	MinItems    *int                  `json:"minItems,omitempty"`
	MaxItems    *int                  `json:"maxItems,omitempty"`
	Default     json.RawMessage       `json:"default,omitempty"`
}

// ElicitationOption is a titled choice. Const is the machine value an answer
// must use; Title is only a label.
type ElicitationOption struct {
	Const       string `json:"const"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// ElicitationEnumItems limits multiselect answers to offered string values,
// either plain (Type and Enum) or titled (AnyOf).
type ElicitationEnumItems struct {
	Type  string              `json:"type,omitempty"`
	Enum  []string            `json:"enum,omitempty"`
	AnyOf []ElicitationOption `json:"anyOf,omitempty"`
}

// ElicitationResponse retains answer JSON until the requested fields validate.
// Unknown actions are readable but never accepted as answers.
type ElicitationResponse struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content"`
}
