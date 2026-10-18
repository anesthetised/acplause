package acp

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// SessionNotification is the params of session/update.
type SessionNotification struct {
	SessionID string
	Update    Update
}

// Update is one session/update payload. Each concrete type supplies its own
// sessionUpdate discriminator.
type Update interface {
	sessionUpdate() string
}

// MarshalJSON writes the update with its discriminator as the first field.
func (n SessionNotification) MarshalJSON() ([]byte, error) {
	body, err := jsonrpc.Marshal(n.Update)
	if err != nil {
		return nil, err
	}
	var update bytes.Buffer
	update.WriteString(`{"sessionUpdate":`)
	update.WriteString(strconv.Quote(n.Update.sessionUpdate()))
	if fields := bytes.TrimPrefix(body, []byte("{")); string(fields) != "}" {
		update.WriteByte(',')
		update.Write(fields)
	} else {
		update.WriteByte('}')
	}
	return jsonrpc.Marshal(struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}{n.SessionID, update.Bytes()})
}

// UserMessage creates or replaces a user message.
type UserMessage struct {
	MessageID string         `json:"messageId"`
	Content   []ContentBlock `json:"content"`
}

// AgentMessage creates or replaces an agent message.
type AgentMessage struct {
	MessageID string         `json:"messageId"`
	Content   []ContentBlock `json:"content"`
}

// AgentThought creates or replaces an agent reasoning message.
type AgentThought struct {
	MessageID string         `json:"messageId"`
	Content   []ContentBlock `json:"content"`
}

// AgentMessageChunk appends to an agent message.
type AgentMessageChunk struct {
	MessageID string       `json:"messageId"`
	Content   ContentBlock `json:"content"`
}

// AgentThoughtChunk appends to an agent reasoning message.
type AgentThoughtChunk struct {
	MessageID string       `json:"messageId"`
	Content   ContentBlock `json:"content"`
}

// State is the session's aggregate foreground state.
type State string

// Foreground states.
const (
	StateIdle    State = "idle"
	StateRunning State = "running"
)

// StopReason says why foreground work ended.
type StopReason string

// Stop reasons.
const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopRefusal         StopReason = "refusal"
	StopCancelled       StopReason = "cancelled"
	StopError           StopReason = "error"
)

// StateUpdate is a snapshot of foreground state. StopReason and Error only
// apply to idle; Error only to StopError.
type StateUpdate struct {
	State      State          `json:"state"`
	StopReason StopReason     `json:"stopReason,omitempty"`
	Error      *jsonrpc.Error `json:"error,omitempty"`
}

// ToolKind categorises a tool for display.
type ToolKind string

// Tool kinds.
const (
	ToolRead    ToolKind = "read"
	ToolEdit    ToolKind = "edit"
	ToolSearch  ToolKind = "search"
	ToolExecute ToolKind = "execute"
	ToolOther   ToolKind = "other"
)

// ToolCallStatus is a tool call's execution status.
type ToolCallStatus string

// Tool call statuses.
const (
	ToolInProgress ToolCallStatus = "in_progress"
	ToolCompleted  ToolCallStatus = "completed"
	ToolFailed     ToolCallStatus = "failed"
	ToolCancelled  ToolCallStatus = "cancelled"
)

// ToolCallUpdate upserts a tool call. It is a patch: omitted fields keep
// their previous values. acplause never clears fields, so zero values are
// simply omitted.
type ToolCallUpdate struct {
	ToolCallID string             `json:"toolCallId"`
	Name       string             `json:"name,omitempty"`
	Title      string             `json:"title,omitempty"`
	Kind       ToolKind           `json:"kind,omitempty"`
	Status     ToolCallStatus     `json:"status,omitempty"`
	Content    []ToolCallContent  `json:"content,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	RawInput   json.RawMessage    `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage    `json:"rawOutput,omitempty"`
}

// Merge applies patch on top of u, field by field.
func (u ToolCallUpdate) Merge(patch ToolCallUpdate) ToolCallUpdate {
	set(&u.Name, patch.Name)
	set(&u.Title, patch.Title)
	set(&u.Kind, patch.Kind)
	set(&u.Status, patch.Status)
	setSlice(&u.Content, patch.Content)
	setSlice(&u.Locations, patch.Locations)
	setSlice(&u.RawInput, patch.RawInput)
	setSlice(&u.RawOutput, patch.RawOutput)
	return u
}

func set[T comparable](dst *T, src T) {
	var zero T
	if src != zero {
		*dst = src
	}
}

func setSlice[S ~[]E, E any](dst *S, src S) {
	if src != nil {
		*dst = src
	}
}

// ToolCallContent is content produced by a tool call. acplause only
// produces regular content blocks.
type ToolCallContent struct {
	Type    string       `json:"type"`
	Content ContentBlock `json:"content"`
}

// ToolText wraps text as tool call content.
func ToolText(s string) ToolCallContent { return ToolCallContent{Type: "content", Content: Text(s)} }

// ToolCallLocation is a file a tool call touches.
type ToolCallLocation struct {
	Path string  `json:"path"`
	Line *uint32 `json:"line,omitempty"`
}

// UsageUpdate reports context window usage and cumulative cost.
type UsageUpdate struct {
	Used uint64 `json:"used"`
	Size uint64 `json:"size"`
	Cost *Cost  `json:"cost,omitempty"`
}

// Cost is an amount of money in an ISO 4217 currency.
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

func (UserMessage) sessionUpdate() string       { return "user_message" }
func (AgentMessage) sessionUpdate() string      { return "agent_message" }
func (AgentThought) sessionUpdate() string      { return "agent_thought" }
func (AgentMessageChunk) sessionUpdate() string { return "agent_message_chunk" }
func (AgentThoughtChunk) sessionUpdate() string { return "agent_thought_chunk" }
func (StateUpdate) sessionUpdate() string       { return "state_update" }
func (ToolCallUpdate) sessionUpdate() string    { return "tool_call_update" }
func (UsageUpdate) sessionUpdate() string       { return "usage_update" }
