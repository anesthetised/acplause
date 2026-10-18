// Package llm is a client for OpenAI-compatible Chat Completions APIs,
// tuned for OpenRouter: streamed responses, tool calls, reasoning and usage
// accounting.
package llm

import "encoding/json"

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Request is one chat completion request.
type Request struct {
	Model    string
	Messages []Message
	Tools    []Tool
	// ReasoningEffort is sent as reasoning.effort when non-empty.
	ReasoningEffort string
}

// Message is a chat message in the OpenAI wire format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Reasoning output of an assistant turn, passed back so models that
	// require it can continue their reasoning across tool calls.
	Reasoning        string            `json:"reasoning,omitempty"`
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`
}

// ReasoningDetail is one structured reasoning block, kept verbatim.
type ReasoningDetail map[string]any

// ToolCall is a model's request to call a tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its JSON-encoded arguments, which
// the model may have produced invalid.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool declares a function the model may call.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function describes a callable tool.
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// FunctionTool builds a Tool of type function.
func FunctionTool(name, description string, parameters json.RawMessage) Tool {
	return Tool{Type: "function", Function: Function{Name: name, Description: description, Parameters: parameters}}
}

// Delta is a piece of streamed output.
type Delta struct {
	Kind DeltaKind
	Text string
}

// DeltaKind tells answer text from reasoning.
type DeltaKind int

// Delta kinds.
const (
	DeltaText DeltaKind = iota
	DeltaReasoning
)

// Finish reasons, as normalised by OpenRouter.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
	FinishError         = "error"
)

// Completion is a fully received response.
type Completion struct {
	Content          string
	Reasoning        string
	ReasoningDetails []ReasoningDetail
	ToolCalls        []ToolCall
	FinishReason     string
	Usage            *Usage
}

// Message returns the assistant message to append to the history.
func (c *Completion) Message() Message {
	m := Message{Role: RoleAssistant, Content: c.Content, ToolCalls: c.ToolCalls}
	if len(c.ReasoningDetails) > 0 {
		m.ReasoningDetails = c.ReasoningDetails
	} else {
		m.Reasoning = c.Reasoning
	}
	return m
}

// Usage is OpenRouter's usage accounting for one request.
type Usage struct {
	PromptTokens     uint64  `json:"prompt_tokens"`
	CompletionTokens uint64  `json:"completion_tokens"`
	TotalTokens      uint64  `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	PromptDetails    struct {
		CachedTokens uint64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens uint64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}
