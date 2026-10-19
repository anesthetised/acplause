package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

// issue50Store leaves ownership-loss discovery under the test's control;
// the regression must not depend on beating the lease-renewal ticker.
type issue50Store struct{ Store }

func (s issue50Store) Renew(context.Context, []string) ([]string, error) { return nil, nil }

// issue50Flush fences the worker's final idle save, which follows its idle
// notification, before another owner is allowed to take the session.
func issue50Flush(t *testing.T, s *Session) {
	t.Helper()
	s.mu.Lock()
	worker := s.worker
	s.mu.Unlock()
	if worker != nil {
		select {
		case <-worker:
		case <-time.After(5 * time.Second):
			t.Fatal("session worker did not finish")
		}
	}
	s.saves.Wait()
	s.flush()
}

func issue50Contains(t *testing.T, label string, value any, markers ...string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range markers {
		if !strings.Contains(string(raw), marker) {
			t.Errorf("%s lost %q", label, marker)
		}
	}
}

func TestIssue50RetakePreservesLatestSession(t *testing.T) {
	for _, observedLoss := range []bool{false, true} {
		name := "unobserved takeover"
		if observedLoss {
			name = "already marked lost"
		}
		t.Run(name, func(t *testing.T) {
			dir, cwd := t.TempDir(), t.TempDir()
			ma, mb := make(scriptedModel, 2), make(scriptedModel, 1)
			wrap := func(st Store) Store { return issue50Store{st} }
			a := startAgent(t, dir, ma, nil, wrap)
			a.initialize()
			id := a.newSession(cwd)
			a.selectModel(id, "m")
			ma.reply("A first answer", llm.FinishStop, &llm.Usage{Cost: 0.125})
			if response := a.await(a.prompt(id, "A first prompt")); response.Error != nil {
				t.Fatal(response.Error)
			}
			a.untilIdle(id)
			old, _ := a.agent.session(id)
			issue50Flush(t, old)

			b := startAgent(t, dir, mb, nil, wrap)
			b.initialize()
			resume := map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}, "replayFrom": map[string]any{"type": "start"}}
			b.ok(acp.MethodSessionResume, resume, nil)
			b.untilIdle(id)
			b.selectModel(id, "r")
			b.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": id, "configId": configEffort, "type": "id", "value": "high"}, nil)
			mb.reply("B unique answer", llm.FinishStop, &llm.Usage{Cost: 0.25})
			if response := b.await(b.prompt(id, "B unique prompt")); response.Error != nil {
				t.Fatal(response.Error)
			}
			b.untilIdle(id)
			current, _ := b.agent.session(id)
			issue50Flush(t, current)
			b.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": id, "configId": "mode", "type": "id", "value": "plan"}, nil)
			issue50Flush(t, current)
			if observedLoss {
				// A write detects loss without the renewal loop evicting the
				// cached Session. Its cancelled context cannot be reused.
				old.flush()
				if !old.isLost() {
					t.Fatal("old session did not observe the takeover")
				}
			}

			response := a.await(a.send(acp.MethodSessionResume, resume))
			if response.Error != nil {
				t.Fatalf("reacquisition must restore a usable session: %v", response.Error)
			}
			replay := a.untilIdle(id)
			for _, marker := range []string{"B unique prompt", "B unique answer"} {
				found := false
				for _, update := range replay {
					found = found || strings.Contains(string(update.Update.Raw), marker)
				}
				if !found {
					t.Errorf("resume transcript lost %q", marker)
				}
			}
			retaken, _ := a.agent.session(id)
			retaken.mu.Lock()
			issue50Contains(t, "memory", retaken.history, "B unique prompt", "B unique answer")
			if retaken.model != "r" || retaken.effort != "high" || retaken.mode != "plan" || retaken.cost != 0.375 {
				t.Errorf("retaken settings/cost: model=%q effort=%q mode=%q cost=%v", retaken.model, retaken.effort, retaken.mode, retaken.cost)
			}
			retaken.mu.Unlock()
			requests := make(chan llm.Request, 1)
			ma <- func(_ context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				onDelta(llm.Delta{Kind: llm.DeltaText, Text: "A third answer"})
				return &llm.Completion{Content: "A third answer", FinishReason: llm.FinishStop, Usage: &llm.Usage{Cost: 0.5}}, nil
			}
			if response := a.await(a.prompt(id, "A third prompt")); response.Error != nil {
				t.Fatal(response.Error)
			}
			a.untilIdle(id)
			issue50Flush(t, retaken)
			select {
			case req := <-requests:
				issue50Contains(t, "next model request", req.Messages, "B unique prompt", "B unique answer")
				if req.Model != "r" || req.ReasoningEffort != "high" {
					t.Errorf("next model settings: model=%q effort=%q", req.Model, req.ReasoningEffort)
				}
			default:
				t.Fatal("retaken session did not call the model")
			}
			loaded, err := a.agent.cfg.Store.Acquire(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			// Convert raw rows to RawMessage so the assertion sees JSON,
			// rather than base64-encoded byte slices.
			for label, rows := range map[string][][]byte{"durable history": loaded.History, "durable transcript": loaded.Transcript} {
				var raw []json.RawMessage
				for _, row := range rows {
					raw = append(raw, row)
				}
				issue50Contains(t, label, raw, "B unique prompt", "B unique answer")
			}
			if loaded.Model != "r" || loaded.Effort != "high" || loaded.Mode != "plan" || loaded.Cost != 0.875 {
				t.Errorf("durable settings/cost: %+v", loaded.Session)
			}
		})
	}
}

func TestIssue50ResumePreservesContinuouslyOwnedWork(t *testing.T) {
	for _, permission := range []bool{false, true} {
		name := "running model"
		if permission {
			name = "pending permission"
		}
		t.Run(name, func(t *testing.T) {
			model := make(scriptedModel, 2)
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			cwd := t.TempDir()
			id := c.newSession(cwd)
			c.selectModel(id, "m")
			entered, release := make(chan struct{}), make(chan struct{})
			var approvals atomic.Int32
			if permission {
				c.mu.Lock()
				c.answer = func(context.Context) (string, string) {
					if approvals.Add(1) == 1 {
						close(entered)
					}
					<-release
					return acp.OutcomeSelected, optionAllow
				}
				c.mu.Unlock()
				model.callTool("original-tool", "write_file", `{"path":"approved","content":"done"}`)
				model.reply("finished original tool", llm.FinishStop, nil)
			} else {
				model <- func(ctx context.Context, _ llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
					close(entered)
					select {
					case <-release:
						onDelta(llm.Delta{Kind: llm.DeltaText, Text: "finished original request"})
						return &llm.Completion{Content: "finished original request", FinishReason: llm.FinishStop}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			if response := c.await(c.prompt(id, "keep original work")); response.Error != nil {
				t.Fatal(response.Error)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("original work did not start")
			}
			state := string(acp.StateRunning)
			if permission {
				state = string(acp.StateRequiresAction)
			}
			c.waitFor(id, func(n notification) bool { return n.Update.State == state })
			original, _ := c.agent.session(id)
			c.ok(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			c.waitFor(id, func(n notification) bool { return n.Update.Kind == "state_update" && n.Update.State == state })
			resumed, _ := c.agent.session(id)
			if resumed != original || original.ctx.Err() != nil {
				t.Error("continuous ownership replaced or cancelled the original worker")
			}
			close(release)
			if idle := c.untilIdle(id); idle[len(idle)-1].Update.StopReason != string(acp.StopEndTurn) {
				t.Fatalf("original work did not finish: %+v", idle)
			}
			issue50Flush(t, resumed)
			if permission {
				if approvals.Load() != 1 {
					t.Errorf("permission requests = %d, want 1", approvals.Load())
				}
				if raw, err := os.ReadFile(filepath.Join(cwd, "approved")); err != nil || string(raw) != "done" {
					t.Errorf("original approval did not complete: %q %v", raw, err)
				}
			}
		})
	}
}
