package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

func assertTurnTimeout(t *testing.T, updates []acp.Update, notice bool) {
	t.Helper()
	idles, notices := 0, 0
	for _, u := range updates {
		switch v := u.(type) {
		case acp.Notice:
			if v.Title == "Turn time limit reached" {
				notices++
				if v.Severity != "warning" || strings.Contains(strings.ToLower(v.Description), "user cancelled") {
					t.Fatalf("misleading timeout notice: %+v", v)
				}
			}
		case acp.StateUpdate:
			if v.State == acp.StateIdle {
				idles++
				if v.StopReason != acp.StopCancelled || v.Error != nil {
					t.Fatalf("timeout changed cancellation semantics: %+v", v)
				}
			}
		}
	}
	if idles != 1 || notices != map[bool]int{true: 1, false: 0}[notice] {
		t.Fatalf("idles=%d notices=%d, want notice=%v", idles, notices, notice)
	}
}

func TestTurnTimeoutResetsForQueuedPromptAndPairsSiblingCalls(t *testing.T) {
	requests := make(chan llm.Request, 2)
	entered := make(chan struct{})
	contexts := make(chan context.Context, 2)
	s, out := testSession(t, fakeModel(func(ctx context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		requests <- req
		contexts <- ctx
		if len(requests) == 1 {
			close(entered)
			<-ctx.Done()
			// Even a response arriving at cancellation must pair every call.
			return &llm.Completion{ToolCalls: []llm.ToolCall{
				{ID: "a", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}},
				{ID: "b", Function: llm.FunctionCall{Name: testShellName(), Arguments: testShellArgs(`touch changed`, `[IO.File]::WriteAllText('changed', '')`)}},
			}}, nil
		}
		if ctx.Err() != nil {
			t.Error("queued turn inherited expired allowance")
		}
		assertToolPairs(t, req.Messages)
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}))
	s.deps.turnTimeout = 500 * time.Millisecond
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("wait")})
	<-entered
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("next")})
	assertTurnTimeout(t, out.untilIdle(t), true)
	second := out.untilIdle(t)
	if idle := second[len(second)-1].(acp.StateUpdate); idle.StopReason != acp.StopEndTurn {
		t.Fatalf("next prompt did not get a new allowance: %+v", idle)
	}
	s.mu.Lock()
	worker := s.worker
	s.mu.Unlock()
	if worker != nil {
		<-worker
	}
	first, next := <-contexts, <-contexts
	if !errors.Is(context.Cause(first), errTurnTimeout) || !errors.Is(context.Cause(next), context.Canceled) {
		t.Fatalf("deadline cause or timer cleanup: first=%v next=%v", context.Cause(first), context.Cause(next))
	}
	if _, err := os.Stat(filepath.Join(s.cwd, "changed")); !os.IsNotExist(err) {
		t.Fatalf("sibling tool ran after timeout: %v", err)
	}
	for _, u := range second {
		if _, ok := u.(acp.Notice); ok {
			t.Fatal("previous timer leaked into next turn")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.transcript {
		if _, ok := u.(acp.Notice); ok {
			t.Fatal("live timeout notice entered replay")
		}
	}
}

func TestTurnTimeoutDisabledHasNoDeadline(t *testing.T) {
	requests := make(chan context.Context, 1)
	s, out := testSession(t, fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		requests <- ctx
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}))
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("unlimited")})
	updates := out.untilIdle(t)
	if _, ok := (<-requests).Deadline(); ok {
		t.Fatal("default created a turn deadline")
	}
	for _, u := range updates {
		if _, ok := u.(acp.Notice); ok {
			t.Fatal("disabled timeout produced a notice")
		}
	}
}

func TestTurnTimeoutInterruptsPermissionAndFormOverProtocol(t *testing.T) {
	for _, form := range []bool{false, true} {
		t.Run(map[bool]string{false: "permission", true: "form"}[form], func(t *testing.T) {
			model := make(scriptedModel, 2)
			if form {
				model.callTool("waiting", "ask_user", questionArgs)
			} else {
				model.callTool("waiting", "write_file", `{"path":"changed","content":"bad"}`)
			}
			requests := make(chan llm.Request, 1)
			model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				assertToolPairs(t, req.Messages)
				return &llm.Completion{FinishReason: llm.FinishStop}, nil
			}
			c := startAgent(t, t.TempDir(), model, nil)
			c.agent.deps.turnTimeout = time.Second
			permission, release := make(chan struct{}, 1), make(chan struct{})
			defer close(release)
			c.answer = func(context.Context) (string, string) {
				permission <- struct{}{}
				<-release
				return acp.OutcomeSelected, optionAllow
			}
			if form {
				c.formInitialize()
			} else {
				c.initialize()
			}
			cwd := t.TempDir()
			sid := c.newSession(cwd)
			c.selectModel(sid, "m")
			c.await(c.prompt(sid, "wait for input"))
			if form {
				c.question()
			} else {
				select {
				case <-permission:
				case <-time.After(5 * time.Second):
					t.Fatal("no permission request")
				}
			}
			updates := c.untilIdle(sid)
			idles, notices := 0, 0
			for _, n := range updates {
				if n.Update.Kind == "notice" {
					notices++
				}
				if n.Update.Kind == "state_update" && n.Update.State == "idle" {
					idles++
				}
			}
			if idle := lastIdle(updates); idle.Update.StopReason != "cancelled" || idle.Update.Error != nil || idles != 1 || notices != 1 || len(requests) != 0 {
				t.Fatalf("timeout protocol: %+v", updates)
			}
			if form {
				select {
				case <-c.cancellations:
				case <-time.After(5 * time.Second):
					t.Fatal("form did not use cancellable request")
				}
			} else {
				select {
				case <-c.cancellations:
					t.Fatal("permission sent form-style cancellation")
				default:
				}
				release <- struct{}{}
			}
			c.await(c.prompt(sid, "continue"))
			c.untilIdle(sid)
			if len(requests) != 1 {
				t.Fatal("next prompt did not run")
			}
			if _, err := os.Stat(filepath.Join(cwd, "changed")); !os.IsNotExist(err) {
				t.Fatalf("timed-out approval wrote: %v", err)
			}
		})
	}
}

func TestTurnTimeoutInterruptsManualAndAutomaticCompaction(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "manual"}[manual], func(t *testing.T) {
			requests := make(chan llm.Request, 1)
			s, out := compactionSession(t, fakeModel(func(ctx context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				<-ctx.Done()
				return nil, ctx.Err()
			}))
			s.deps.turnTimeout = 500 * time.Millisecond
			old := strings.Repeat("old answer ", 1000)
			s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "old request"}, llm.Message{Role: llm.RoleAssistant, Content: old})
			prompt := "continue"
			if manual {
				prompt = "/compact"
			}
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text(prompt)})
			updates := out.untilIdle(t)
			assertTurnTimeout(t, updates, true)
			if req := <-requests; len(req.Tools) != 0 || req.Messages[0].Content != summaryInstruction {
				t.Fatal("fixture did not exercise summary waiting")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.history[2].Content != old || len(s.history) != 4 {
				t.Fatal("timed-out compaction installed partial history")
			}
			statuses := []acp.CompactionStatus{}
			for _, u := range updates {
				if c, ok := u.(acp.CompactionUpdate); ok {
					statuses = append(statuses, c.Status)
				}
			}
			if len(statuses) != 2 || statuses[0] != acp.CompactionInProgress || statuses[1] != acp.CompactionCancelled {
				t.Fatalf("compaction lifecycle: %v", statuses)
			}
		})
	}
}

func TestTurnTimeoutStartsAfterDurableInsertion(t *testing.T) {
	entered, release := make(chan context.Context, 1), make(chan struct{})
	defer close(release)
	requests := make(chan context.Context, 1)
	s, out := testSession(t, fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		requests <- ctx
		if _, ok := ctx.Deadline(); !ok {
			t.Error("model work has no turn deadline")
		}
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}))
	s.deps.turnTimeout = time.Second
	s.saves.Wait() // testSession's asynchronous model-selection save must finish first
	var first atomic.Bool
	s.deps.store = saveStore{save: func(ctx context.Context, row store.Session, _, _ []store.Row) error {
		if row.State == string(acp.StateRunning) && first.CompareAndSwap(false, true) {
			entered <- ctx
			<-release
		}
		return ctx.Err()
	}}
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("persist first")})
	ctx := <-entered
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("database insertion consumed turn timeout")
	}
	release <- struct{}{}
	if idle := out.untilIdle(t); idle[len(idle)-1].(acp.StateUpdate).StopReason != acp.StopEndTurn || len(requests) != 1 {
		t.Fatal("post-insertion work did not run")
	}
}

func TestTurnTimeoutParentCancellationWinsAtFinalization(t *testing.T) {
	for _, kind := range []string{"user", "parent", "ownership"} {
		t.Run(kind, func(t *testing.T) {
			expired, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			s, out := testSession(t, fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				<-ctx.Done()
				close(expired)
				<-release
				return nil, ctx.Err()
			}))
			s.deps.turnTimeout = 500 * time.Millisecond
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("wait")})
			<-expired
			switch kind {
			case "user":
				s.Cancel()
			case "parent":
				s.cancel()
			case "ownership":
				s.mu.Lock()
				s.loseLocked()
				s.mu.Unlock()
			}
			release <- struct{}{}
			assertTurnTimeout(t, out.untilIdle(t), false)
		})
	}
}
