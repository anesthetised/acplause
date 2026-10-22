package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/mcp"
	"github.com/anesthetised/acplause/internal/tools"
)

// converse runs model rounds until the model stops calling tools, the round
// budget runs out or the turn fails. The caller turns any result into
// cancelled when ctx is done.
func (s *Session) converse(ctx context.Context) (acp.StopReason, *jsonrpc.Error) {
	var remaining *int // nil is an unlimited request budget, including summaries
	if s.deps.maxRounds > 0 {
		remaining = new(s.deps.maxRounds)
	}
	for {
		if err := s.budget.check(ctx); err != nil {
			return acp.StopError, turnError(err)
		}
		if remaining != nil && *remaining == 0 {
			return acp.StopMaxTurnRequests, nil
		}
		req, err := s.prepareRequest(ctx, remaining)
		if errors.Is(err, errRoundLimit) {
			return acp.StopMaxTurnRequests, nil
		}
		if err != nil {
			return acp.StopError, turnError(err)
		}
		if remaining != nil {
			*remaining--
		}
		out := roundOutput{s: s}
		if err := s.budget.check(ctx); err != nil {
			return acp.StopError, turnError(err)
		}
		completion, err := s.deps.model.Chat(ctx, req.Request, out.delta)
		if err != nil {
			return acp.StopError, turnError(err)
		}
		s.budget.record(completion.Usage)
		s.reportUsage(req.Model, completion.Usage)
		s.appendHistory(completion.Message())
		s.flush()

		s.runTools(ctx, completion.ToolCalls, req)
		if err := s.budget.check(ctx); err != nil {
			return acp.StopError, turnError(err)
		}
		if len(completion.ToolCalls) == 0 {
			return stopReason(completion.FinishReason)
		}
	}
}

// request snapshots what the next model request needs. The image policy is
// taken with the model, so a selection change cannot mix the two.
func (s *Session) request() modelRound {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := llm.Request{Model: s.model, Messages: slices.Clone(s.history), Vision: s.catalog.vision(s.model)}
	for _, spec := range s.toolbox.Specs() {
		if !allowsTool(s.mode, spec.Name) {
			continue
		}
		req.Tools = append(req.Tools, llm.FunctionTool(spec.Name, spec.Description, spec.Parameters))
	}
	req.Tools = append(req.Tools, updatePlanTool)
	if s.supportsForms() {
		req.Tools = append(req.Tools, askUserTool)
	}
	if s.catalog.reasons(s.model) && s.effort != effortDefault {
		req.ReasoningEffort = s.effort
	}
	bindings := map[string]mcp.Tool{}
	for _, t := range s.mcp.Snapshot() {
		if s.mode == modePlan {
			continue
		}
		bindings[t.Name] = t
		req.Tools = append(req.Tools, llm.FunctionTool(t.Name, t.Description, t.InputSchema))
	}
	return modelRound{Request: req, bindings: bindings, mode: s.mode}
}

// toolOutcome keeps completion unpublished until the worker commits its model
// result. Progress may already be visible; the toolbox stays pinned by the caller.
type toolOutcome struct {
	entry   int
	final   acp.ToolCallUpdate
	message llm.Message
}

// runTool remains serial: execution and its history commit finish before the
// next call starts. One root snapshot covers both steps.
func (s *Session) runTool(ctx context.Context, call llm.ToolCall, round modelRound) {
	toolbox := s.acquireToolbox()
	defer s.releaseToolbox(toolbox)
	s.commitToolOutcome(s.executeToolCall(ctx, call, round, toolbox))
}

// executeToolCall uses normal preparation, policy and progress reporting, but
// returns the final patch and matching result without committing either.
// The caller owns the toolbox pin and must commit each outcome exactly once.
func (s *Session) executeToolCall(ctx context.Context, call llm.ToolCall, round modelRound, toolbox *tools.Toolbox) toolOutcome {
	if ctx.Err() != nil {
		return toolOutcome{message: toolResult(call.ID, "Cancelled before it ran.")}
	}

	name := call.Function.Name
	update := acp.ToolCallUpdate{
		ToolCallID: newID(),
		Name:       name,
		Title:      name,
		Kind:       toolbox.Kind(name),
		RawInput:   rawInput(call.Function.Arguments),
	}
	var result toolOutput
	entry := -1
	var prepared *preparedTool
	err := s.budget.check(ctx)
	if err != nil {
		// Pair the call using the normal failed-result path, without preparing
		// it or starting permission, forms, shell or MCP work.
	} else if !allowsTool(round.mode, name) {
		err = fmt.Errorf("tool %q is unavailable in Plan mode", name)
	} else if name == "update_plan" {
		prepared, err = s.preparePlan(call.Function.Arguments)
	} else if name == "ask_user" && s.supportsForms() {
		var req acp.ElicitationRequest
		req, err = parseQuestions(call.Function.Arguments)
		req.SessionID, req.ToolCallID = s.id, update.ToolCallID
		prepared = &preparedTool{Title: req.Message, run: func(ctx context.Context) toolOutput { return s.askUser(ctx, req) }}
	} else if binding, ok := round.bindings[name]; ok {
		if binding.ReadOnly {
			update.Kind = acp.ToolRead
		}
		prepared, err = prepareMCP(binding, call.Function.Arguments, round.Vision)
	} else {
		prepared, err = prepareBuiltin(toolbox, name, call.Function.Arguments)
	}
	permissionCtx, stopPermission := context.WithCancel(ctx)
	defer stopPermission()
	if prepared != nil && prepared.binding != nil {
		// Definition revocation stops permission waiting. The MCP transport
		// separately admits dispatch; execution must not inherit this context.
		stopBinding := context.AfterFunc(prepared.binding, stopPermission)
		defer stopBinding()
		if prepared.binding.Err() != nil {
			err = errors.New("MCP tool binding was revoked")
		}
	}
	if err == nil {
		update.Title = prepared.Title
		update.Locations = locations(prepared.Locations)
		if name == "ask_user" {
			update.Status = acp.ToolPending
			entry = s.reportTool(entry, update)
		} else if prepared.permission {
			entry, err = s.requestPermission(permissionCtx, ctx, entry, &update, prepared)
		}
	}
	if err != nil {
		result = toolOutput{Output: "Error: " + err.Error(), Failed: true}
	} else {
		patch := acp.ToolCallUpdate{ToolCallID: update.ToolCallID, Status: acp.ToolInProgress}
		if entry < 0 {
			patch = update
			patch.Status = acp.ToolInProgress
		}
		update.Status = acp.ToolInProgress
		// Shell progress is reported with its terminal reference after process start.
		if name != "ask_user" && prepared.observeShell == nil {
			entry = s.reportTool(entry, patch)
		}
		runCtx := ctx
		if prepared.binding != nil && s.deps.mcpTimeout > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(ctx, s.deps.mcpTimeout)
			defer cancel()
		}
		result = s.executeTool(runCtx, prepared, entry, update.ToolCallID)
	}

	final := acp.ToolCallUpdate{
		ToolCallID: update.ToolCallID,
		Status:     acp.ToolCompleted,
		Content:    result.Content,
		RawOutput:  result.RawOutput,
	}
	switch {
	case ctx.Err() != nil:
		final = acp.ToolCallUpdate{ToolCallID: update.ToolCallID, Status: acp.ToolCancelled}
		result.Output = "Cancelled by the user."
		result.Images = nil
	case result.Failed:
		final.Status = acp.ToolFailed

	}
	if ctx.Err() == nil {
		if len(final.Content) == 0 {
			final.Content = []acp.ToolCallContent{acp.ToolText(result.Output)}
		}
		if final.RawOutput == nil {
			final.RawOutput = jsonString(result.Output)
		}
	}
	if update.Status == "" { // never reported: send everything at once
		final = update.Merge(final)
	}
	message := toolResult(call.ID, result.Output)
	message.Images = result.Images
	return toolOutcome{entry: entry, final: final, message: message}
}

// commitToolOutcome publishes completion and history under one lock, so a
// concurrent save or resume cannot see a completed card without its result.
// Cancellation after execution does not rewrite an already produced outcome.
func (s *Session) commitToolOutcome(outcome toolOutcome) {
	s.mu.Lock()
	if outcome.final.ToolCallID != "" {
		s.reportToolLocked(outcome.entry, outcome.final)
	}
	s.history = append(s.history, outcome.message)
	s.updatedAt = time.Now()
	s.mu.Unlock()
	if outcome.final.ToolCallID != "" {
		s.flush()
	}
}

// asksPermission reports whether tools of a kind change the workspace and so
// run only with the user's permission (ADR 0003).
func asksPermission(kind acp.ToolKind) bool {
	return kind == acp.ToolEdit || kind == acp.ToolExecute
}

// Permission option ids. Only bash offers "always" options (ADR 0029); other
// tools keep ADR 0003's once-only choices.
const (
	optionAllow  = "allow"
	optionReject = "reject"
)

var permissionOptions = []acp.PermissionOption{
	{OptionID: optionAllow, Name: "Allow", Kind: acp.PermissionAllowOnce},
	{OptionID: optionReject, Name: "Reject", Kind: acp.PermissionRejectOnce},
}

var (
	errRejected           = errors.New("the user rejected this tool call")
	errRejectedRemembered = errors.New("the user rejected this exact command earlier in this session with \"always reject\"")
)

// requestPermission reports the call as pending, with the diff it would
// apply, and asks the user to allow it. A nil error is approval; any other
// answer, or none, is an error for the model. A call whose preview fails is
// not asked about: it would fail anyway.
func (s *Session) requestPermission(ctx, turnCtx context.Context, entry int, update *acp.ToolCallUpdate, call *preparedTool) (int, error) {
	diff, err := call.Preview(ctx)
	if err == nil {
		err = ctx.Err() // cancelled during the preview: nothing left to ask about
	}
	if err != nil {
		return entry, err
	}
	update.Status = acp.ToolPending
	if diff != nil {
		update.Content = []acp.ToolCallContent{*diff}
	}
	if call.Command != "" {
		if allow, ok := s.remembered(call.Command); ok {
			s.logRemembered(call.Command, allow)
			if allow {
				update.Title += " (allowed earlier this session)"
			}
			entry = s.reportTool(entry, *update)
			if allow {
				return entry, nil
			}
			return entry, errRejectedRemembered
		}
	}
	entry = s.reportTool(entry, *update)

	subject := &acp.PermissionSubject{Type: acp.SubjectToolCall, ToolCall: update}
	if call.Command != "" {
		subject = &acp.PermissionSubject{Type: acp.SubjectCommand, Command: call.Command, Cwd: s.cwd, ToolCallID: update.ToolCallID}
	}
	options := permissionOptions
	always, gen := false, uint64(0)
	if call.Command != "" {
		always, gen = s.canRemember()
	}
	if always {
		options = shellPermissionOptions
	}
	req := acp.RequestPermissionRequest{SessionID: s.id, Title: call.Title + "?", Subject: subject, Options: options}

	// Tools run one at a time, so waiting blocks all foreground work.
	s.setState(acp.StateUpdate{State: acp.StateRequiresAction})
	s.flush() // a blocked turn is busy: it cannot be taken over
	var resp acp.RequestPermissionResponse
	// Call sends even when ctx has ended; a dialog for a cancelled call would
	// ask about work that never runs.
	if err = ctx.Err(); err == nil {
		err = s.deps.out.Call(ctx, acp.MethodSessionRequestPermission, req, &resp)
	}
	if turnCtx.Err() != nil {
		return entry, turnCtx.Err() // the turn ends as cancelled; no running in between
	}
	s.setState(acp.StateUpdate{State: acp.StateRunning})
	switch outcome := resp.Outcome; {
	case err != nil:
		return entry, fmt.Errorf("could not get the user's permission: %v", err)
	case outcome.Outcome == acp.OutcomeSelected && outcome.OptionID == optionAllow:
		return entry, nil
	case outcome.Outcome == acp.OutcomeSelected && outcome.OptionID == optionReject:
		return entry, errRejected
	case outcome.Outcome == acp.OutcomeSelected && outcome.OptionID == optionAllowAlways && always:
		s.remember(call.Command, true, gen)
		return entry, nil
	case outcome.Outcome == acp.OutcomeSelected && outcome.OptionID == optionRejectAlways && always:
		s.remember(call.Command, false, gen)
		return entry, errRejected
	}
	return entry, fmt.Errorf("permission was not granted (outcome %q, option %q)", resp.Outcome.Outcome, resp.Outcome.OptionID)
}

// reportTool updates replay and live output atomically, including calls
// still in progress when the client resumes.
func (s *Session) reportTool(entry int, patch acp.ToolCallUpdate) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reportToolLocked(entry, patch)
}

// reportToolLocked also serves the atomic outcome commit. Requires s.mu.
func (s *Session) reportToolLocked(entry int, patch acp.ToolCallUpdate) int {
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
	if total := s.cost + u.Cost; u.Cost >= 0 && !math.IsNaN(total) && !math.IsInf(total, 0) {
		s.cost = total
	}
	cost := s.cost

	size := s.catalog.contextLength(model)
	if size == 0 {
		return // usage_update requires the window size
	}
	used, ok := u.ReportedTokens()
	if !ok {
		return // missing counts or a wrapped sum must not reset the context meter
	}
	s.send(acp.UsageUpdate{
		Used: used,
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

// allowsTool is shared by advertisement and invocation. Plan uses names,
// never shell parsing or external readOnlyHint annotations (ADR 0013).
func allowsTool(mode, name string) bool {
	if mode != modePlan {
		return true
	}
	switch name {
	case "read_file", "list_dir", "search_code", "get_instructions", "ask_user", "update_plan":
		return true
	}
	return false
}
