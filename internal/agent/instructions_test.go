package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

func writeInstructions(t *testing.T, cwd, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInstructionsRefreshEachModelRound(t *testing.T) {
	for _, mode := range []string{modeBuild, modePlan} {
		t.Run(mode, func(t *testing.T) {
			var s *Session
			requests := make(chan llm.Request, 3)
			model := fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				switch len(requests) {
				case 1:
					writeInstructions(t, s.cwd, "Second root marker.")
				case 2:
					if err := os.Remove(filepath.Join(s.cwd, "AGENTS.md")); err != nil {
						t.Error(err)
					}
				default:
					return &llm.Completion{FinishReason: llm.FinishStop}, nil
				}
				return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "lookup", Function: llm.FunctionCall{Name: "get_instructions", Arguments: `{"path":"."}`}}}}, nil
			})
			var out recorder
			s, out = testSession(t, model)
			setSessionMode(t, s, mode)
			writeInstructions(t, s.cwd, "First root marker.")
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("inspect")})
			updates := out.untilIdle(t)
			if idle := updates[len(updates)-1].(acp.StateUpdate); idle.StopReason != acp.StopEndTurn {
				t.Fatalf("turn: %+v", idle)
			}
			for i, want := range []string{"First root marker.", "Second root marker.", ""} {
				req := <-requests
				text := req.Messages[0].Content
				if (want != "" && !strings.Contains(text, want)) || (i > 0 && strings.Contains(text, "First root marker.")) || (i == 2 && strings.Contains(text, "Second root marker.")) {
					t.Errorf("round %d root instructions: %s", i, text)
				}
				if mode == modePlan && !strings.Contains(text, "Plan mode") {
					t.Error("refresh lost mode")
				}
				if i > 0 {
					result := req.Messages[len(req.Messages)-1]
					if result.Role != llm.RoleTool || !strings.Contains(result.Content, "precedence") {
						t.Errorf("scoped tool failed: %+v", result)
					}
				}
			}
		})
	}
}

// instructionReadBarrier pauses the error path after the filesystem read,
// before publication. Resume and cancellation must remain available here.
type instructionReadBarrier struct {
	slog.Handler
	read    chan struct{}
	release chan struct{}
}

func (b instructionReadBarrier) Enabled(context.Context, slog.Level) bool { return true }

func (b instructionReadBarrier) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "skipping AGENTS.md" {
		b.read <- struct{}{}
		<-b.release
	}
	return nil
}

func TestInstructionsRefreshResumeAndCancelBarrier(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel"}[cancel], func(t *testing.T) {
			testInstructionsRefreshBarrier(t, cancel, false)
		})
	}
}

func testInstructionsRefreshBarrier(t *testing.T, cancel, replaceRoot bool) {
	requests := make(chan llm.Request, 1)
	s, out := testSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		requests <- req
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}))
	barrier := instructionReadBarrier{Handler: slog.DiscardHandler, read: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(barrier.release)
	s.deps.log = slog.New(barrier)
	writeInstructions(t, s.cwd, string([]byte{0xff}))
	s.instructions = "Previous marker."
	old := s.toolbox
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("inspect")})
	select {
	case <-barrier.read:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach instruction read barrier")
	}
	if cancel {
		s.Cancel()
	} else {
		if replaceRoot {
			// Physical replacement of an open root is a Unix-only fixture.
			if err := os.Rename(s.cwd, filepath.Join(t.TempDir(), "retired")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(s.cwd, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		// In-place content replacement remains valid with Windows open roots.
		// The read already captured invalid old text before its barrier, so a
		// roots-generation retry must observe this fresh content before modeling.
		writeInstructions(t, s.cwd, "Current generation marker.")
		if err := s.Resume(context.Background(), []string{t.TempDir()}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	barrier.release <- struct{}{}
	updates := out.untilIdle(t)
	idle := updates[len(updates)-1].(acp.StateUpdate)
	if cancel {
		if idle.StopReason != acp.StopCancelled || len(requests) != 0 {
			t.Fatalf("cancelled read reached model: %+v", idle)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.instructions != "Previous marker." || s.activeToolbox != nil {
			t.Fatal("cancelled read published or retained roots")
		}
	} else {
		if idle.StopReason != acp.StopEndTurn {
			t.Fatalf("resumed read: %+v", idle)
		}
		req := <-requests
		if !strings.Contains(req.Messages[0].Content, "Current generation marker.") || strings.Contains(req.Messages[0].Content, "Previous marker.") {
			t.Fatalf("stale instructions published: %s", req.Messages[0].Content)
		}
		call, err := old.Prepare("list_dir", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := call.Run(context.Background()); !got.Failed {
			t.Fatal("retired toolbox retained after refresh")
		}
	}
}

func TestInstructionsReadFailureWarnsAndOmitsOldText(t *testing.T) {
	var s *Session
	requests := make(chan llm.Request, 2)
	s, out := testSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		requests <- req
		if len(requests) == 1 {
			writeInstructions(t, s.cwd, "Secret marker.\xff")
			return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "list", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}}}}, nil
		}
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}))
	var log bytes.Buffer
	s.deps.log = slog.New(slog.NewTextHandler(&log, nil))
	writeInstructions(t, s.cwd, "Old root marker.")
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("inspect")})
	out.untilIdle(t)
	first, second := <-requests, <-requests
	if !strings.Contains(first.Messages[0].Content, "Old root marker.") || strings.Contains(second.Messages[0].Content, "Old root marker.") || strings.Contains(second.Messages[0].Content, "Secret marker.") {
		t.Fatal("root read failure retained instructions")
	}
	if !strings.Contains(log.String(), "skipping AGENTS.md") || strings.Contains(log.String(), "Secret marker.") {
		t.Fatalf("unsafe or missing warning: %s", log.String())
	}
}

func TestInstructionsCompactRefreshUsesStableSnapshot(t *testing.T) {
	var s *Session
	summaries := make(chan struct{}, 10)
	answers := make(chan llm.Request, 1)
	s, out := compactionSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		if len(req.Tools) > 0 {
			answers <- req
			return &llm.Completion{FinishReason: llm.FinishStop}, nil
		}
		summaries <- struct{}{}
		s.mu.Lock()
		text := s.history[0].Content
		s.mu.Unlock()
		if !strings.Contains(text, "Compaction snapshot marker.") || strings.Contains(text, "Next round marker.") {
			t.Error("compaction refreshed in the middle of rolling summaries")
		}
		writeInstructions(t, s.cwd, "Next round marker.")
		return &llm.Completion{Content: "Older work summarized.", FinishReason: llm.FinishStop}, nil
	}))
	s.deps.maxRounds = 0
	for range 3 {
		s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "old prompt"}, llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("old answer ", 700)})
	}
	writeInstructions(t, s.cwd, "Compaction snapshot marker.")
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("/compact")})
	updates := out.untilIdle(t)
	if idle := updates[len(updates)-1].(acp.StateUpdate); idle.StopReason != acp.StopEndTurn || len(summaries) < 2 {
		t.Fatalf("rolling summary not exercised: %+v, summaries=%d", idle, len(summaries))
	}
	s.mu.Lock()
	if !strings.Contains(s.history[0].Content, "Compaction snapshot marker.") {
		t.Error("manual compaction did not refresh root instructions")
	}
	s.mu.Unlock()
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("continue")})
	out.untilIdle(t)
	if req := <-answers; !strings.Contains(req.Messages[0].Content, "Next round marker.") || strings.Contains(req.Messages[0].Content, "Compaction snapshot marker.") {
		t.Fatalf("next normal round kept old snapshot: %s", req.Messages[0].Content)
	}
}

func TestInstructionsScopedHistoryAndRootRefreshAfterRestore(t *testing.T) {
	model := make(scriptedModel, 3)
	model.callTool("scoped", "get_instructions", `{"path":"src"}`)
	model.reply("inspected", llm.FinishStop, nil)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	writeInstructions(t, cwd, "Original root marker.")
	if err := os.Mkdir(filepath.Join(cwd, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeInstructions(t, filepath.Join(cwd, "src"), "Scoped replay marker.")
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("inspect")}}, nil)
	c.untilIdle(sid)
	c.ok(acp.MethodSessionClose, map[string]any{"sessionId": sid}, nil)
	writeInstructions(t, cwd, "Restored root marker.")
	writeInstructions(t, filepath.Join(cwd, "src"), "Changed scoped marker.")
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := c.untilIdle(sid)
	found := false
	for _, n := range replay {
		if n.Update.Kind == "tool_call_update" && strings.Contains(string(n.Update.Raw), "Scoped replay marker.") {
			found = true
		}
	}
	if !found {
		t.Fatal("scoped tool result was not replayed")
	}
	requests := make(chan llm.Request, 1)
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		requests <- req
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}
	c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("continue")}}, nil)
	c.untilIdle(sid)
	req := <-requests
	if !strings.Contains(req.Messages[0].Content, "Restored root marker.") || strings.Contains(req.Messages[0].Content, "Original root marker.") || !strings.Contains(req.Messages[3].Content, "Scoped replay marker.") || strings.Contains(req.Messages[3].Content, "Changed scoped marker.") {
		t.Fatalf("restore mixed live instructions and durable history: %+v", req.Messages)
	}
}

func TestInstructionsRefreshLeavesProtocolReaderResponsive(t *testing.T) {
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.initialize()
	barrier := instructionReadBarrier{Handler: slog.DiscardHandler, read: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(barrier.release)
	c.agent.deps.log = slog.New(barrier)
	cwd := t.TempDir()
	writeInstructions(t, cwd, "\xff")
	// Session setup does not read instructions. The first worker read pauses
	// only after insertion; the same ACP connection can still cancel the turn.
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("inspect")}}, nil)
	select {
	case <-barrier.read:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach instruction read barrier")
	}
	c.ok(acp.MethodSessionCancel, map[string]any{"sessionId": sid}, nil)
	barrier.release <- struct{}{}
	if idle := lastIdle(c.untilIdle(sid)); idle.Update.StopReason != "cancelled" || idle.Update.Error != nil {
		t.Fatalf("refresh cancellation: %+v", idle)
	}
}
