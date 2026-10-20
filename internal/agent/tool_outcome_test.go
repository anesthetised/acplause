package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

type outcomeClient struct {
	recorder
	cancel context.CancelFunc
}

func (c *outcomeClient) Call(ctx context.Context, method string, params, result any) error {
	err := c.recorder.Call(ctx, method, params, result)
	if c.cancel != nil {
		c.cancel()
	}
	return err
}

// outcomeSnapshot exercises the persistence encoding, including typed images,
// without a database or a second worker changing the state under inspection.
func outcomeSnapshot(t *testing.T, s *Session) *store.Loaded {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded := &store.Loaded{Session: s.record()}
	loaded.State = string(acp.StateRunning)
	for _, m := range s.history[1:] {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		loaded.History = append(loaded.History, raw)
	}
	for _, u := range s.transcript {
		raw, err := encodeEntry(u)
		if err != nil {
			t.Fatal(err)
		}
		loaded.Transcript = append(loaded.Transcript, raw)
	}
	return loaded
}

func TestToolOutcomesCommitMixedResultsInCallOrder(t *testing.T) {
	s, out := imageSession(t, noModel(t), 0)
	client := &outcomeClient{recorder: out}
	s.deps.out = client
	img := imageBlock(t, testImage(t, 3, 2))
	cfg := imagePeer(t, func(*http.Request) any { return map[string]any{"content": []any{img}} })
	if err := s.Resume(t.Context(), nil, []acp.MCPServer{cfg}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cwd, "a.txt"), []byte("read marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	round := s.request()
	alias := s.mcp.Snapshot()[0].Name
	calls := []llm.ToolCall{
		{ID: "read", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
		{ID: "bad", Function: llm.FunctionCall{Name: "missing_tool", Arguments: `{}`}},
		{ID: "image", Function: llm.FunctionCall{Name: alias, Arguments: `{}`}},
		{ID: "cancelled", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}},
		{ID: "skipped", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}},
	}
	s.appendHistory(llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
	tb := s.acquireToolbox()
	defer s.releaseToolbox(tb)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var outcomes []toolOutcome
	for i, call := range calls {
		if i == 3 {
			client.cancel = cancel
		}
		outcome := s.executeToolCall(ctx, call, round, tb)
		outcomes = append(outcomes, outcome)
		if len(s.history) != 2 {
			t.Fatal("execution committed model history before its outcome")
		}
		if outcome.message.ToolCallID != call.ID {
			t.Fatal("outcome lost upstream call identity")
		}
		if outcome.final.ToolCallID != "" {
			for _, u := range s.transcript {
				if patch, ok := u.(acp.ToolCallUpdate); ok && patch.ToolCallID == outcome.final.ToolCallID && patch.Status != acp.ToolPending && patch.Status != acp.ToolInProgress {
					t.Fatal("execution published completion before history commit")
				}
			}
		}
	}
	if !strings.Contains(outcomes[0].message.Content, "read marker") || outcomes[1].final.Status != acp.ToolFailed || len(outcomes[2].message.Images) != 1 || outcomes[3].final.Status != acp.ToolCancelled || outcomes[4].message.Content != "Cancelled before it ran." || outcomes[4].final.ToolCallID != "" {
		t.Fatal("mixed outcomes lost success, failure, image or cancellation semantics")
	}
	if len(outcomes[3].message.Images) != 0 || outcomes[3].message.Content != "Cancelled by the user." {
		t.Fatal("cancelled execution leaked its result")
	}
	if _, err := os.Stat(filepath.Join(s.cwd, "changed")); !os.IsNotExist(err) {
		t.Fatalf("cancelled approval wrote: %v", err)
	}
	// An executed but uncommitted result must recover as interrupted, not be
	// replayed or leave a completed card without a matching history result.
	pending := outcomeSnapshot(t, s)
	for _, outcome := range outcomes {
		s.commitToolOutcome(outcome)
	}
	assertToolPairs(t, s.history)
	for i, call := range calls {
		if m := s.history[i+2]; m.ToolCallID != call.ID || m.Content != outcomes[i].message.Content {
			t.Fatal("history changed call order or result")
		}
	}
	if imageCount(s.history) != 1 {
		t.Fatal("commit lost typed image")
	}
	for i, outcome := range outcomes[:4] {
		found := false
		for _, u := range s.transcript {
			if patch, ok := u.(acp.ToolCallUpdate); ok && patch.ToolCallID == outcome.final.ToolCallID {
				found = patch.Status == outcome.final.Status
				if i == 2 && strings.Contains(string(patch.RawOutput), img.Data) {
					t.Fatal("image bytes escaped into rawOutput")
				}
			}
		}
		if !found {
			t.Fatal("commit lost original ACP identity or status")
		}
	}
	for _, snapshot := range []*store.Loaded{pending, outcomeSnapshot(t, s)} {
		restored, err := restoreSession(t.Context(), deps{}, s.catalog, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		assertToolPairs(t, restored.history)
		if len(restored.history) != 7 {
			t.Fatal("recovery duplicated or omitted results")
		}
		for i, call := range calls {
			got := restored.history[i+2]
			if got.ToolCallID != call.ID {
				t.Fatal("recovery reordered results")
			}
			if snapshot == pending && (got.Content != interrupted || len(got.Images) != 0) {
				t.Fatal("uncommitted outcome survived as completed")
			}
			if snapshot != pending && got.Content != outcomes[i].message.Content {
				t.Fatal("recovery replaced a committed result")
			}
		}
		restored.repair()
		if len(restored.history) != 7 {
			t.Fatal("repair duplicated results")
		}
		restored.Close()
	}
}

func TestToolOutcomeCommitPersistsFinalAndResultTogether(t *testing.T) {
	s, _ := testSession(t, noModel(t))
	s.saves.Wait()
	call := llm.ToolCall{ID: "read", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}}
	s.appendHistory(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}})
	tb := s.acquireToolbox()
	defer s.releaseToolbox(tb)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outcome := s.executeToolCall(ctx, call, s.request(), tb)
	saves := 0
	s.deps.store = saveStore{save: func(_ context.Context, _ store.Session, history, transcript []store.Row) error {
		saves++
		if len(history) != 2 || len(transcript) != 1 {
			t.Fatalf("split outcome save: history=%d transcript=%d", len(history), len(transcript))
		}
		var result llm.Message
		if err := json.Unmarshal(history[1].Data, &result); err != nil {
			t.Fatal(err)
		}
		patch, err := decodeEntry(transcript[0].Data)
		if err != nil {
			t.Fatal(err)
		}
		if result.ToolCallID != call.ID || patch.(acp.ToolCallUpdate).ToolCallID != outcome.final.ToolCallID || patch.(acp.ToolCallUpdate).Status != acp.ToolCompleted {
			t.Fatal("save lost completion or matching result")
		}
		return nil
	}}
	// Cancellation cannot erase an already produced success while it awaits its
	// ordered commit, or the live/durable result would depend on sibling speed.
	cancel()
	s.commitToolOutcome(outcome)
	if saves != 1 {
		t.Fatalf("commit saves=%d, want 1", saves)
	}
}

func TestToolOutcomePreparationKeepsPolicy(t *testing.T) {
	for _, policy := range []string{"Plan", "budget", "arguments"} {
		t.Run(policy, func(t *testing.T) {
			s, out := budgetSession(t, noModel(t), 0, 0)
			call := llm.ToolCall{ID: "blocked", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}}
			round := s.request()
			want := "Plan mode"
			switch policy {
			case "Plan":
				round.mode = modePlan
			case "budget":
				s.budget = &turnBudget{maxTokens: 1}
				s.budget.record(budgetUsage(1, 0))
				want = "budget reached"
			case "arguments":
				call.Function.Name = "read_file"
				call.Function.Arguments = `{}`
				want = "path"
			}
			tb := s.acquireToolbox()
			defer s.releaseToolbox(tb)
			outcome := s.executeToolCall(t.Context(), call, round, tb)
			if outcome.final.Status != acp.ToolFailed || !strings.Contains(outcome.message.Content, want) || len(s.history) != 1 || out.permissions.Load() != 0 {
				t.Fatalf("preparation bypassed %s: %+v", policy, outcome)
			}
			if _, err := os.Stat(filepath.Join(s.cwd, "changed")); !os.IsNotExist(err) {
				t.Fatalf("blocked preparation wrote: %v", err)
			}
		})
	}
}
