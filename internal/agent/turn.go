package agent

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/tools"
)

// converse runs model rounds until the model stops calling tools, the round
// budget runs out or the turn fails. The caller turns any result into
// cancelled when ctx is done.
func (s *Session) converse(ctx context.Context) (acp.StopReason, *jsonrpc.Error) {
	for round := 0; ; round++ {
		if round == s.deps.maxRounds {
			return acp.StopMaxTurnRequests, nil
		}
		req := s.request()
		out := roundOutput{s: s}
		completion, err := s.deps.model.Chat(ctx, req, out.delta)
		out.record()
		if err != nil {
			return acp.StopError, turnError(err)
		}
		s.reportUsage(req.Model, completion.Usage)
		s.appendHistory(completion.Message())

		if len(completion.ToolCalls) == 0 {
			return stopReason(completion.FinishReason)
		}
		for _, call := range completion.ToolCalls {
			s.runTool(ctx, call)
		}
		if ctx.Err() != nil {
			return acp.StopCancelled, nil
		}
	}
}

// request snapshots what the next model request needs.
func (s *Session) request() llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := llm.Request{Model: s.model, Messages: slices.Clone(s.history)}
	for _, spec := range s.toolbox.Specs() {
		req.Tools = append(req.Tools, llm.FunctionTool(spec.Name, spec.Description, spec.Parameters))
	}
	if s.catalog.reasons(s.model) && s.effort != effortDefault {
		req.ReasoningEffort = s.effort
	}
	return req
}

// runTool reports and executes one tool call. Every call gets a result in
// the history, so the conversation stays valid even when cancelled.
func (s *Session) runTool(ctx context.Context, call llm.ToolCall) {
	if ctx.Err() != nil {
		s.appendHistory(toolResult(call.ID, "Cancelled before it ran."))
		return
	}
	toolbox := s.acquireToolbox()
	defer s.releaseToolbox(toolbox)

	name := call.Function.Name
	update := acp.ToolCallUpdate{
		ToolCallID: newID(),
		Name:       name,
		Title:      name,
		Kind:       toolbox.Kind(name),
		RawInput:   rawInput(call.Function.Arguments),
	}
	var result tools.Result
	if prepared, err := toolbox.Prepare(name, call.Function.Arguments); err != nil {
		result = tools.Result{Output: "Error: " + err.Error(), Failed: true}
	} else {
		update.Title = prepared.Title
		update.Locations = locations(prepared.Locations)
		update.Status = acp.ToolInProgress
		s.send(update)
		result = prepared.Run(ctx)
	}

	final := acp.ToolCallUpdate{
		ToolCallID: update.ToolCallID,
		Status:     acp.ToolCompleted,
		Content:    []acp.ToolCallContent{acp.ToolText(result.Output)},
		RawOutput:  jsonString(result.Output),
	}
	switch {
	case ctx.Err() != nil:
		final = acp.ToolCallUpdate{ToolCallID: update.ToolCallID, Status: acp.ToolCancelled}
		result.Output = "Cancelled by the user."
	case result.Failed:
		final.Status = acp.ToolFailed
	}
	if update.Status == "" { // never reported: send everything at once
		final = update.Merge(final)
	}
	s.send(final)
	s.record(update.Merge(final))
	s.appendHistory(toolResult(call.ID, result.Output))
}

func (s *Session) reportUsage(model string, u *llm.Usage) {
	if u == nil {
		return
	}
	s.mu.Lock()
	s.cost += u.Cost
	cost := s.cost
	s.mu.Unlock()

	size := s.catalog.contextLength(model)
	if size == 0 {
		return // usage_update requires the window size
	}
	s.send(acp.UsageUpdate{
		Used: u.PromptTokens + u.CompletionTokens,
		Size: size,
		Cost: &acp.Cost{Amount: cost, Currency: "USD"},
	})
}

// roundOutput streams one model response as an agent thought and an agent
// message, each with its own id, and records them for replay.
type roundOutput struct {
	s       *Session
	thought message
	answer  message
}

type message struct {
	id   string
	text strings.Builder
}

func (o *roundOutput) delta(d llm.Delta) {
	m := &o.answer
	if d.Kind == llm.DeltaReasoning {
		m = &o.thought
	}
	if m.id == "" {
		m.id = newID()
	}
	m.text.WriteString(d.Text)
	if d.Kind == llm.DeltaReasoning {
		o.s.send(acp.AgentThoughtChunk{MessageID: m.id, Content: acp.Text(d.Text)})
	} else {
		o.s.send(acp.AgentMessageChunk{MessageID: m.id, Content: acp.Text(d.Text)})
	}
}

func (o *roundOutput) record() {
	if o.thought.id != "" {
		o.s.record(acp.AgentThought{MessageID: o.thought.id, Content: []acp.ContentBlock{acp.Text(o.thought.text.String())}})
	}
	if o.answer.id != "" {
		o.s.record(acp.AgentMessage{MessageID: o.answer.id, Content: []acp.ContentBlock{acp.Text(o.answer.text.String())}})
	}
}

func stopReason(finish string) (acp.StopReason, *jsonrpc.Error) {
	switch finish {
	case llm.FinishLength:
		return acp.StopMaxTokens, nil
	case llm.FinishContentFilter:
		return acp.StopRefusal, nil
	case llm.FinishError:
		return acp.StopError, jsonrpc.NewError(jsonrpc.CodeInternalError, "the model reported an error")
	}
	return acp.StopEndTurn, nil
}

// errorData details a model failure in state_update errors.
type errorData struct {
	Kind   llm.ErrorKind `json:"kind"`
	Status int           `json:"status,omitempty"`
}

func turnError(err error) *jsonrpc.Error {
	e, ok := errors.AsType[*llm.Error](err)
	if !ok {
		return jsonrpc.NewError(jsonrpc.CodeInternalError, "%v", err)
	}
	code := jsonrpc.CodeInternalError
	if e.Kind == llm.KindAuth {
		code = acp.CodeAuthRequired
	}
	return &jsonrpc.Error{Code: code, Message: e.Message, Data: errorData{Kind: e.Kind, Status: e.Status}}
}

func toolResult(callID, output string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: callID, Content: output}
}

func locations(paths []string) []acp.ToolCallLocation {
	var locs []acp.ToolCallLocation
	for _, p := range paths {
		locs = append(locs, acp.ToolCallLocation{Path: p})
	}
	return locs
}
