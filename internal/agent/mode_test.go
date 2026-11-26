package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		{ID: "batch", Function: llm.FunctionCall{Name: "edit_batch", Arguments: `{}`}},
		{ID: "shell-read", Function: llm.FunctionCall{Name: testShellName(), Arguments: testShellArgs(`cat AGENTS.md; touch changed`, `[Console]::Write([IO.File]::ReadAllText('AGENTS.md')); [IO.File]::WriteAllText('changed', '')`)}},
		{ID: "external", Function: llm.FunctionCall{Name: "external", Arguments: `{}`}},
		{ID: "unknown", Function: llm.FunctionCall{Name: "_future", Arguments: `{}`}},
	} {
		s.runTool(context.Background(), call, round)
		update := (<-out).(acp.ToolCallUpdate)
		if update.Status != acp.ToolFailed || !strings.Contains(string(update.RawOutput), "Plan mode") {
			t.Fatalf("forbidden call: %+v", update)
		}
		if u := <-out; u != sessionInfoPatch("", s.updatedAt) {
			t.Fatalf("tool result metadata: %+v", u)
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
	if !slices.ContainsFunc(s.request().Tools, func(tool llm.Tool) bool { return tool.Function.Name == testShellName() }) {
		t.Fatal("Build tools not restored")
	}
}

func TestModeBusyAdmissionAndIdempotence(t *testing.T) {
	for _, busy := range []string{"claimed turn", "queue", "state", "setup"} {
		t.Run(busy, func(t *testing.T) {
			s, _ := testSession(t, nil)
			setupPending := busy == "setup"
			switch busy {
			case "claimed turn":
				s.claimed = true
			case "queue":
				s.queue = []*submission{{}}
			case "state":
				s.state.State = acp.StateRequiresAction
			}
			defer func() { s.claimed = false; s.queue = nil }()
			_, err := s.setConfigOption(context.Background(), configMode, acp.ConfigValueID, jsonString(modePlan), setupPending)
			if err == nil || jsonrpc.AsError(err).Code != -32602 || !strings.Contains(err.Error(), "retry") {
				t.Fatalf("busy mode accepted: %v", err)
			}
			if s.mode != "build" {
				t.Fatal("busy change mutated mode")
			}
			if _, err := s.setConfigOption(context.Background(), configMode, acp.ConfigValueID, jsonString(modeBuild), setupPending); err != nil {
				t.Fatalf("same mode refused: %v", err)
			}
		})
	}
}

// renewStore counts lease renewals and loses nothing.
type renewStore struct {
	Store
	renewals atomic.Int32
}

func (s *renewStore) Renew(context.Context, []string) ([]string, error) {
	s.renewals.Add(1)
	return nil, nil
}

func TestModeAdmissionWaitsWithoutTheAgentLock(t *testing.T) {
	s, out := testSession(t, nil)
	st := &renewStore{}
	a := &Agent{
		cfg:        Config{Store: st},
		ctx:        t.Context(),
		deps:       deps{replays: s.deps.replays},
		sessions:   map[string]*Session{s.id: s},
		operations: map[string]*sessionOperation{},
	}
	client := &slowReplayClient{recorder: out, entered: make(chan struct{}), release: make(chan struct{})}
	s.deps.out = client
	unblock := sync.OnceFunc(func() { close(client.release) })
	defer unblock()
	go s.setState(acp.StateUpdate{State: acp.StateRunning})
	issue50Wait(t, client.entered)

	// A live state update holds Session.mu while its output is blocked.
	// Reserving the mode setter and renewing leases must still finish.
	admitted := make(chan struct{})
	go func() {
		defer close(admitted)
		req := acp.SetConfigOptionRequest{SessionID: s.id, ConfigID: configMode, Type: acp.ConfigValueID, Value: jsonString(modePlan)}
		if _, err := a.setConfigOption(t.Context(), req); err != nil {
			t.Error(err)
		}
		a.renew()
	}()
	issue50Wait(t, admitted)
	if st.renewals.Load() != 1 {
		t.Fatalf("renewals = %d, want 1", st.renewals.Load())
	}
	a.mu.Lock()
	op := a.operations[s.id]
	a.mu.Unlock()
	if op == nil {
		t.Fatal("mode setter settled before it could check foreground state")
	}
	unblock()
	issue50Wait(t, op.done)
	if op.err == nil || !strings.Contains(op.err.Error(), "busy") {
		t.Fatalf("mode change during a turn: %v", op.err)
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
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	model := make(scriptedModel, 3)
	model.callTool("blocked", testShellName(), testShellArgs(`touch changed`, `[IO.File]::WriteAllText('changed', '')`))
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
	change := c.send(acp.MethodSessionSetConfig, set("build"))
	same := c.send(acp.MethodSessionSetConfig, set("plan"))
	prompt := c.send(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("after setup")}})
	later := c.send(acp.MethodSessionSetConfig, set("build"))
	c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
	c.fenceReader()
	unblock()
	if r := c.await(resume); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, pending := range []<-chan rpcResponse{change, later} {
		if r := c.await(pending); r.Error == nil || r.Error.Code != -32602 {
			t.Fatalf("busy mode: %+v", r)
		}
	}
	if r := c.await(same); r.Error != nil {
		t.Fatalf("same mode refused: %v", r.Error)
	}
	if r := c.await(prompt); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("queued prompt: %+v", r)
	}
}

func TestModeChangeWhileTheFinishedTurnSaves(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	st := &saveStore{save: func(_ context.Context, sess store.Session, _, _ []store.Row) error {
		if sess.State == string(acp.StateIdle) {
			once.Do(func() { close(entered); <-release }) // only the turn's final save
		}
		return nil
	}}
	out := make(recorder, 32)
	model := fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		return &llm.Completion{Content: "done", FinishReason: llm.FinishStop}, nil
	})
	s, err := newSession(context.Background(), deps{out: out, model: model, store: st}, newCatalog([]llm.Model{{ID: "m"}}), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.model = "m"
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("hi")})
	out.untilIdle(t)
	<-entered
	defer close(release)
	if err := s.SetConfigOption(configMode, acp.ConfigValueID, jsonString(modePlan)); err != nil {
		t.Fatalf("mode change after idle: %v", err)
	}
}
