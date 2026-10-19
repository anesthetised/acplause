package acp

// RequestPermissionRequest is the params of session/request_permission.
type RequestPermissionRequest struct {
	SessionID string             `json:"sessionId"`
	Title     string             `json:"title"` // permission copy; does not retitle the tool call
	Subject   *PermissionSubject `json:"subject,omitempty"`
	Options   []PermissionOption `json:"options"`
}

// Permission subject types acplause sends.
const (
	SubjectToolCall = "tool_call"
	SubjectCommand  = "command"
)

// PermissionSubject is the operation awaiting permission: a tool call, or a
// command with its absolute working directory.
type PermissionSubject struct {
	Type       string          `json:"type"`
	ToolCall   *ToolCallUpdate `json:"toolCall,omitempty"`   // tool_call
	Command    string          `json:"command,omitempty"`    // command
	Cwd        string          `json:"cwd,omitempty"`        // command
	ToolCallID string          `json:"toolCallId,omitempty"` // command
}

// Permission option kinds acplause offers.
const (
	PermissionAllowOnce  = "allow_once"
	PermissionRejectOnce = "reject_once"
)

// PermissionOption is one answer the user may choose.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// OutcomeSelected is the outcome of a request the user answered. Any other
// outcome, cancelled or unknown, is never approval.
const OutcomeSelected = "selected"

// RequestPermissionResponse is the client's answer.
type RequestPermissionResponse struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId"` // selected
	} `json:"outcome"`
}
