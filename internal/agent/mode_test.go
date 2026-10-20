package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/mcp"
	"github.com/anesthetised/acplause/internal/store"
)

func setSessionMode(t *testing.T, s *Session, mode string) {
	t.Helper()
	if err := s.SetConfigOption("mode", acp.ConfigValueID, jsonString(mode)); err != nil {
		t.Fatal(err)
	}
}

func TestModeToolsAndRoundAdmission(t *testing.T) {
	s, out := testSession(t, nil)
	setSessionMode(t, s, "plan")
	round := s.request()
	var names []string
	for _, tool := range round.Tools {
		names = append(names, tool.Function.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"get_instructions", "list_dir", "read_file", "search_code", "update_plan"}) {
		t.Fatalf("Plan tools: %v", names)
	}
	// A later session selection cannot change the policy of an old round.
	setSessionMode(t, s, "build")
	round.bindings["external"] = mcp.Tool{Name: "external", ReadOnly: true}
	for _, call := range []llm.ToolCall{
		{ID: "write", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}},
		{ID: "edit", Function: llm.FunctionCall{Name: "edit", Arguments: `{}`}},
		{ID: "shell-read", Function: llm.FunctionCall{Name: "bash", Arguments: `{"command":"cat AGENTS.md; touch changed"}`}},
		{ID: "external", Function: llm.FunctionCall{Name: "external", Arguments: `{}`}},
		{ID: "unknown", Function: llm.FunctionCall{Name: "_future", Arguments: `{}`}},
	} {
		s.runTool(context.Background(), call, round)
		update := (<-out).(acp.ToolCallUpdate)
		if update.Status != acp.ToolFailed || !strings.Contains(string(update.RawOutput), "Plan mode") {
			t.Fatalf("forbidden call: %+v", update)
		}
		result := s.history[len(s.history)-1]
		if result.Role != llm.RoleTool || result.ToolCallID != call.ID {
			t.Fatalf("missing result: %+v", result)
		}
	}
	if _, err := os.Stat(filepath.Join(s.cwd, "changed")); !os.IsNotExist(err) {
		t.Fatalf("Plan side effect: %v", err)
	}
	s.deps.forms = new(atomic.Bool)
	s.deps.forms.Store(true)
	setSessionMode(t, s, "plan")
	if !slices.ContainsFunc(s.request().Tools, func(tool llm.Tool) bool { return tool.Function.Name == "ask_user" }) {
		t.Fatal("Plan lost questions on capable client")
	}
	setSessionMode(t, s, "build")
	if !slices.ContainsFunc(s.request().Tools, func(tool llm.Tool) bool { return tool.Function.Name == "bash" }) {
		t.Fatal("Build tools not restored")
	}
}

func TestModeBusyAdmissionAndIdempotence(t *testing.T) {
	for _, busy := range []string{"worker", "queue", "state", "setup"} {
		t.Run(busy, func(t *testing.T) {
			s, _ := testSession(t, nil)
			a := &Agent{sessions: map[string]*Session{s.id: s}, operations: map[string]*sessionOperation{}}
			switch busy {
			case "worker":
				s.worker = make(chan struct{})
			case "queue":
				s.queue = []*submission{{}}
			case "state":
				s.state.State = acp.StateRequiresAction
			case "setup":
				a.operations[s.id] = &sessionOperation{}
			}
			defer func() { s.worker = nil; s.queue = nil }()
			req := acp.SetConfigOptionRequest{SessionID: s.id, ConfigID: "mode", Type: "id", Value: jsonString("plan")}
			_, err := a.setConfigOption(context.Background(), req)
			if err == nil || jsonrpc.AsError(err).Code != -32602 || !strings.Contains(err.Error(), "retry") {
				t.Fatalf("busy mode accepted: %v", err)
			}
			if s.mode != "build" {
				t.Fatal("busy change mutated mode")
			}
			req.Value = jsonString("build")
			if _, err := a.setConfigOption(context.Background(), req); err != nil {
				t.Fatalf("same mode refused: %v", err)
			}
		})
	}
}

func TestModePromptRebuiltAndInvalidValueAtomic(t *testing.T) {
	s, _ := testSession(t, nil)
	s.instructions = "Retain project rules"
	extra := t.TempDir()
	if err := s.Resume(context.Background(), []string{extra}, nil, false); err != nil {
		t.Fatal(err)
	}
	build := s.history[0].Content
	setSessionMode(t, s, "plan")
	plan := s.history[0].Content
	for _, part := range []string{s.cwd, extra, s.instructions, "Plan mode", "Do not claim"} {
		if !strings.Contains(plan, part) {
			t.Fatalf("system prompt lost %q: %s", part, plan)
		}
	}
	for _, raw := range []json.RawMessage{jsonString("unknown"), []byte(`42`), []byte(`null`)} {
		if err := s.SetConfigOption("mode", "id", raw); err == nil {
			t.Fatalf("invalid mode accepted: %s", raw)
		}
		if s.mode != "plan" || s.history[0].Content != plan {
			t.Fatal("invalid mode mutated session")
		}
	}
	setSessionMode(t, s, "build")
	if s.history[0].Content != build {
		t.Fatal("Build prompt not restored")
	}
}

// modeSetupStore keeps a resume reservation live while the ACP reader runs.
type modeSetupStore struct {
	Store
	release <-chan struct{}
}

func (s modeSetupStore) Acquire(ctx context.Context, id string) (*store.Loaded, error) {
	select {
	case <-s.release:
		return s.Store.Acquire(ctx, id)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestModeConfigOverWireFencesSetupAndPreservesPreferences(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	model := make(scriptedModel, 3)
	model.callTool("blocked", "bash", `{"command":"touch changed"}`)
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		result := req.Messages[len(req.Messages)-1]
		if result.Role != llm.RoleTool || result.ToolCallID != "blocked" || !strings.Contains(result.Content, "Plan mode") {
			t.Errorf("Plan result: %+v", result)
		}
		return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "read", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}}}}, nil
	}
	model.reply("planned", llm.FinishStop, nil)
	c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store { return modeSetupStore{Store: st, release: release} })
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	// A mode-only change in this older session must not overwrite the newer
	// model/effort preference chosen in a different session.
	other := c.newSession(cwd)
	c.selectModel(other, "r")
	c.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": other, "configId": configEffort, "type": "id", "value": "high"}, nil)
	set := func(value string) map[string]any {
		return map[string]any{"sessionId": sid, "configId": "mode", "type": "id", "value": value}
	}
	var options acp.SetConfigOptionResponse
	c.ok(acp.MethodSessionSetConfig, set("plan"), &options)
	if len(options.ConfigOptions) != 2 || options.ConfigOptions[1].Category != "mode" || options.ConfigOptions[1].CurrentValue != "plan" {
		t.Fatalf("mode config: %+v", options)
	}
	c.fails(acp.MethodSessionSetConfig, set("unknown"), -32602)
	var fresh acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, map[string]any{"cwd": cwd}, &fresh)
	if fresh.ConfigOptions[0].CurrentValue != "r" || fresh.ConfigOptions[1].CurrentValue != "high" || fresh.ConfigOptions[2].CurrentValue != "build" {
		t.Fatalf("mode polluted preferences: %+v", fresh)
	}

	c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("plan")}}, nil)
	updates := c.untilIdle(sid)
	failed, completed := 0, 0
	for _, u := range updates {
		if u.Update.Kind == "tool_call_update" {
			if u.Update.Status == "failed" {
				failed++
			}
			if u.Update.Status == "completed" {
				completed++
			}
		}
	}
	if failed != 1 || completed != 1 {
		t.Fatalf("Plan tool loop: %+v", updates)
	}
	if _, err := os.Stat(filepath.Join(cwd, "changed")); !os.IsNotExist(err) {
		t.Fatalf("Plan side effect: %v", err)
	}

	resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd})
	c.fails(acp.MethodSessionSetConfig, set("build"), -32602)
	c.ok(acp.MethodSessionSetConfig, set("plan"), nil)
	prompt := c.send(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("after setup")}})
	c.fails(acp.MethodSessionSetConfig, set("build"), -32602)
	// Cancel the deferred prompt while resume still owns the reservation.
	c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
	// A later request is a read-loop barrier for notification processing.
	c.ok(acp.MethodSessionSetConfig, set("plan"), nil)
	// Let the defer release setup; verify cancellation in a separate cleanup.
	t.Cleanup(func() {
		if r := c.await(resume); r.Error != nil {
			t.Errorf("resume: %+v", r.Error)
		}
		if r := c.await(prompt); r.Error == nil || r.Error.Code != -32800 {
			t.Errorf("queued prompt: %+v", r.Error)
		}
	})
}
