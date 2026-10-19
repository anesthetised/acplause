package agent

import (
	"context"
	"errors"
	"fmt"
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
		if err != nil {
			return acp.StopError, turnError(err)
		}
		s.reportUsage(req.Model, completion.Usage)
		s.appendHistory(completion.Message())
		s.flush()

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
	entry := -1
	prepared, err := toolbox.Prepare(name, call.Function.Arguments)
	if err == nil {
		update.Title = prepared.Title
		update.Locations = locations(prepared.Locations)
		if asksPermission(update.Kind) {
			entry, err = s.requestPermission(ctx, entry, &update, prepared)
		}
	}
	if err != nil {
		result = tools.Result{Output: "Error: " + err.Error(), Failed: true}
	} else {
		patch := acp.ToolCallUpdate{ToolCallID: update.ToolCallID, Status: acp.ToolInProgress}
		if entry < 0 {
			patch = update
			patch.Status = acp.ToolInProgress
		}
		update.Status = acp.ToolInProgress
		entry = s.reportTool(entry, patch)
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
	case result.Diff != nil:
		final.Content = []acp.ToolCallContent{*result.Diff, final.Content[0]}
	}
	if update.Status == "" { // never reported: send everything at once
		final = update.Merge(final)
	}
	s.reportTool(entry, final)
	s.appendHistory(toolResult(call.ID, result.Output))
	s.flush()
}

// asksPermission reports whether tools of a kind change the workspace and so
// run only with the user's permission (ADR 0003).
func asksPermission(kind acp.ToolKind) bool {
	return kind == acp.ToolEdit || kind == acp.ToolExecute
}

// Permission option ids. Only "once" options are offered: remembering
// choices is the client's auto-approval (ADR 0003).
const (
	optionAllow  = "allow"
	optionReject = "reject"
)

var permissionOptions = []acp.PermissionOption{
	{OptionID: optionAllow, Name: "Allow", Kind: acp.PermissionAllowOnce},
	{OptionID: optionReject, Name: "Reject", Kind: acp.PermissionRejectOnce},
}

var errRejected = errors.New("the user rejected this tool call")

// requestPermission reports the call as pending, with the diff it would
// apply, and asks the user to allow it. A nil error is approval; any other
// answer, or none, is an error for the model. A call whose preview fails is
// not asked about: it would fail anyway.
func (s *Session) requestPermission(ctx context.Context, entry int, update *acp.ToolCallUpdate, call *tools.Call) (int, error) {
	diff, err := call.Preview(ctx)
	if err != nil {
		return entry, err
	}
	update.Status = acp.ToolPending
	if diff != nil {
		update.Content = []acp.ToolCallContent{*diff}
	}
	entry = s.reportTool(entry, *update)

	subject := &acp.PermissionSubject{Type: acp.SubjectToolCall, ToolCall: update}
	if call.Command != "" {
		subject = &acp.PermissionSubject{Type: acp.SubjectCommand, Command: call.Command, Cwd: s.cwd, ToolCallID: update.ToolCallID}
	}
	req := acp.RequestPermissionRequest{SessionID: s.id, Title: call.Title + "?", Subject: subject, Options: permissionOptions}

	// Tools run one at a time, so waiting blocks all foreground work.
	s.setState(acp.StateUpdate{State: acp.StateRequiresAction})
	s.flush() // a blocked turn is busy: it cannot be taken over
	var resp acp.RequestPermissionResponse
	err = s.deps.out.Call(ctx, acp.MethodSessionRequestPermission, req, &resp)
	if ctx.Err() != nil {
		return entry, ctx.Err() // the turn ends as cancelled; no running in between
	}
	s.setState(acp.StateUpdate{State: acp.StateRunning})
	switch outcome := resp.Outcome; {
	case err != nil:
		return entry, fmt.Errorf("could not get the user's permission: %v", err)
	case outcome.Outcome == acp.OutcomeSelected && outcome.OptionID == optionAllow:
		return entry, nil
	case outcome.Outcome == acp.OutcomeSelected && outcome.OptionID == optionReject:
		return entry, errRejected
	}
	return entry, fmt.Errorf("permission was not granted (outcome %q, option %q)", resp.Outcome.Outcome, resp.Outcome.OptionID)
}

// reportTool updates replay and live output atomically, including calls
// still in progress when the client resumes.
func (s *Session) reportTool(entry int, patch acp.ToolCallUpdate) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry < 0 {
		entry = len(s.transcript)
		s.transcript = append(s.transcript, patch)
	} else {
		s.transcript[entry] = s.transcript[entry].(acp.ToolCallUpdate).Merge(patch)
	}
	s.dirty[entry] = true
	s.send(patch)
	return entry
}

func (s *Session) reportUsage(model string, u *llm.Usage) {
	if u == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cost += u.Cost
	cost := s.cost

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
// message, each with its own id and replay entry updated with every chunk.
type roundOutput struct {
	s       *Session
	thought message
	answer  message
}

type message struct {
	id    string
	entry int // index of this message's current snapshot in the transcript
	text  strings.Builder
}

func (o *roundOutput) delta(d llm.Delta) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	m := &o.answer
	if d.Kind == llm.DeltaReasoning {
		m = &o.thought
	}
	if m.id == "" {
		m.id = newID()
		m.entry = len(o.s.transcript)
		o.s.transcript = append(o.s.transcript, nil)
	}
	m.text.WriteString(d.Text)
	o.s.dirty[m.entry] = true
	if d.Kind == llm.DeltaReasoning {
		o.s.transcript[m.entry] = acp.AgentThought{MessageID: m.id, Content: []acp.ContentBlock{acp.Text(m.text.String())}}
		o.s.send(acp.AgentThoughtChunk{MessageID: m.id, Content: acp.Text(d.Text)})
	} else {
		o.s.transcript[m.entry] = acp.AgentMessage{MessageID: m.id, Content: []acp.ContentBlock{acp.Text(m.text.String())}}
		o.s.send(acp.AgentMessageChunk{MessageID: m.id, Content: acp.Text(d.Text)})
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
