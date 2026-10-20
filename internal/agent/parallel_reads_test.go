package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/mcp"
	"github.com/anesthetised/acplause/internal/store"
)

func readCalls(n int) []llm.ToolCall {
	calls := make([]llm.ToolCall, n)
	for i := range calls {
		calls[i] = llm.ToolCall{ID: fmt.Sprint(i), Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}}
	}
	return calls
}

func TestParallelReadEligibilityIsExactAndBuiltin(t *testing.T) {
	round := modelRound{bindings: map[string]mcp.Tool{"read_file": {ReadOnly: true}, "mcp_read": {ReadOnly: true}}}
	for _, name := range []string{"list_dir", "search_code", "get_instructions", "read_file", "mcp_read", "bash", "write_file", "edit", "edit_batch", "ask_user", "update_plan", "unknown"} {
		want := name == "list_dir" || name == "search_code" || name == "get_instructions"
		if got := parallelRead(llm.ToolCall{Function: llm.FunctionCall{Name: name}}, round); got != want {
			t.Errorf("%s eligible=%v want=%v", name, got, want)
		}
	}
	delete(round.bindings, "read_file")
	if !parallelRead(llm.ToolCall{Function: llm.FunctionCall{Name: "read_file"}}, round) {
		t.Fatal("builtin read excluded")
	}
}

func TestParallelReadBoundOrderedPrefixAndCancellation(t *testing.T) {
	for _, workers := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			calls := readCalls(workers + 2)
			entered := make(chan string, len(calls))
			gates := make(map[string]chan struct{})
			for _, call := range calls {
				gates[call.ID] = make(chan struct{})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var active, maximum atomic.Int32
			committed := make(chan string, len(calls))
			done := make(chan struct{})
			go func() {
				defer close(done)
				orderedReads(ctx, calls, workers, func(call llm.ToolCall) toolOutcome {
					if ctx.Err() != nil {
						return toolOutcome{message: toolResult(call.ID, "Cancelled before it ran.")}
					}
					n := active.Add(1)
					for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
					}
					entered <- call.ID
					select {
					case <-gates[call.ID]:
					case <-ctx.Done():
					}
					active.Add(-1)
					return toolOutcome{message: toolResult(call.ID, "finished")}
				}, func(out toolOutcome) { committed <- out.message.ToolCallID })
			}()
			seen := map[string]bool{}
			for range workers {
				select {
				case id := <-entered:
					seen[id] = true
				case <-time.After(5 * time.Second):
					t.Fatal("reads did not overlap to configured bound")
				}
			}
			if !seen["0"] || maximum.Load() != int32(workers) {
				t.Fatal("incorrect admission window")
			}
			// Later outcomes can finish first, but cannot commit ahead of call zero.
			for id := range seen {
				if id != "0" {
					close(gates[id])
				}
			}
			select {
			case id := <-committed:
				t.Fatalf("out-of-order prefix %s", id)
			default:
			}
			close(gates["0"])
			select {
			case id := <-committed:
				if id != "0" {
					t.Fatal("history reordered")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("prefix did not commit")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not join reads")
			}
			if active.Load() != 0 || maximum.Load() > int32(workers) || len(committed) != len(calls)-1 {
				t.Fatal("lost siblings or exceeded bound")
			}
			for _, call := range calls[1:] {
				if id := <-committed; id != call.ID {
					t.Fatalf("commit order %s want %s", id, call.ID)
				}
			}
		})
	}
}

func TestParallelReadPartialCrashKeepsOnlyCommittedPrefix(t *testing.T) {
	s, _ := testSession(t, noModel(t))
	calls := readCalls(4)
	s.appendHistory(llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
	tb := s.acquireToolbox()
	defer s.releaseToolbox(tb)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocked, ready, prefix := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("read group did not join")
		}
	}()
	go func() {
		defer close(done)
		orderedReads(ctx, calls, 2, func(call llm.ToolCall) toolOutcome {
			if call.ID == "1" {
				close(blocked)
				<-ctx.Done()
			}
			outcome := s.executeToolCall(ctx, call, s.request(), tb)
			if call.ID == "2" {
				close(ready)
			}
			return outcome
		}, func(out toolOutcome) {
			s.commitToolOutcome(out)
			if out.message.ToolCallID == "0" {
				close(prefix)
			}
		})
	}()
	for _, gate := range []<-chan struct{}{blocked, prefix, ready} {
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
			t.Fatal("partial group barrier not reached")
		}
	}
	snapshot := outcomeSnapshot(t, s)
	if len(snapshot.History) != 2 {
		t.Fatal("later completion escaped ordered prefix")
	}
	restored, err := restoreSession(t.Context(), deps{}, s.catalog, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	assertToolPairs(t, restored.history)
	if len(restored.history) != 6 || restored.history[2].Content == interrupted {
		t.Fatal("committed prefix lost")
	}
	for i, call := range calls[1:] {
		got := restored.history[i+3]
		if got.ToolCallID != call.ID || got.Content != interrupted {
			t.Fatal("unfinished group was replayed or reordered")
		}
	}
	restored.repair()
	if len(restored.history) != 6 {
		t.Fatal("repair duplicated a result")
	}
}

func TestParallelReadResumePinsGroupAndDrainsBeforeBarriers(t *testing.T) {
	s, _ := testSession(t, noModel(t))
	s.saves.Wait()
	s.deps.readConcurrency = 2
	if err := os.WriteFile(filepath.Join(s.cwd, "marker"), []byte("old root"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := []llm.ToolCall{
		{ID: "0", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"marker"}`}},
		{ID: "1", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"marker"}`}},
		{ID: "barrier", Function: llm.FunctionCall{Name: "unknown", Arguments: `{}`}},
		{ID: "new", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"marker"}`}},
	}
	s.appendHistory(llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
	saved, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.deps.store = saveStore{save: func(context.Context, store.Session, []store.Row, []store.Row) error {
		once.Do(func() { close(saved); <-release })
		return nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer func() {
		cancel()
		close(release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("group failed to drain")
		}
	}()
	old := s.toolbox
	go func() { defer close(done); s.runTools(ctx, calls, s.request()) }()
	select {
	case <-saved:
	case <-time.After(5 * time.Second):
		t.Fatal("no prefix save")
	}
	if err := os.Rename(s.cwd, filepath.Join(t.TempDir(), "retired")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cwd, "marker"), []byte("new root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(t.Context(), []string{t.TempDir()}, nil, false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pinned := s.activeToolbox == old
	s.mu.Unlock()
	if !pinned {
		t.Fatal("resume released group pin")
	}
	call, err := old.Prepare("read_file", `{"path":"marker"}`)
	if err != nil || call.Run(t.Context()).Output != "old root" {
		t.Fatal("retired roots closed before group joined")
	}
	// Only the first result may be committed while its save is held; the
	// barrier and the following read cannot move ahead of the read group.
	s.mu.Lock()
	count := len(s.history)
	s.mu.Unlock()
	if count != 3 {
		t.Fatal("work crossed the group/save barrier")
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resume stalled read group")
	}
	s.saves.Wait()
	assertToolPairs(t, s.history)
	if s.history[2].Content != "old root" || s.history[3].Content != "old root" || !strings.Contains(s.history[4].Content, "unknown tool") || s.history[5].Content != "new root" {
		t.Fatal("group mixed root generations or crossed a barrier")
	}
	if result := call.Run(t.Context()); !result.Failed {
		t.Fatal("retired roots retained after group")
	}
}

type benchmarkReadClient struct{ recorder }

func (benchmarkReadClient) Notify(string, any) error { return nil }

func BenchmarkParallelRead(b *testing.B) {
	cwd := b.TempDir()
	data := []byte(strings.Repeat("representative search line\n", 2048))
	for i := range 16 {
		if err := os.WriteFile(filepath.Join(cwd, fmt.Sprintf("%02d.txt", i)), data, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	for _, name := range []string{"read_file", "search_code"} {
		for _, workers := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("%s/workers=%d", name, workers), func(b *testing.B) {
				s, err := newSession(b.Context(), deps{out: benchmarkReadClient{}, readConcurrency: workers}, newCatalog(nil), cwd, nil)
				if err != nil {
					b.Fatal(err)
				}
				defer s.Close()
				calls := readCalls(8)
				for i := range calls {
					calls[i].Function = llm.FunctionCall{Name: name, Arguments: fmt.Sprintf(`{"path":"%02d.txt"}`, i)}
					if name == "search_code" {
						calls[i].Function.Arguments = `{"pattern":"absent benchmark marker"}`
					}
				}
				b.ReportAllocs()
				for b.Loop() {
					s.history = s.history[:1]
					s.transcript = nil
					s.dirty = map[int]bool{}
					s.runTools(b.Context(), calls, modelRound{})
				}
			})
		}
	}
}

type parallelPermissionClient struct {
	recorder
	s           *Session
	t           *testing.T
	permissions int
}

func (c *parallelPermissionClient) Call(ctx context.Context, method string, params, result any) error {
	c.permissions++
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if len(c.s.history) != 4 {
		c.t.Fatal("permission crossed preceding reads or allowed later history")
	}
	for _, u := range c.s.transcript {
		if patch, ok := u.(acp.ToolCallUpdate); ok && patch.Name == "get_instructions" {
			c.t.Fatal("later read crossed MCP permission barrier")
		}
	}
	return c.recorder.Call(ctx, method, params, result)
}

func TestParallelReadMCPReadOnlyStillRequiresSerialPermission(t *testing.T) {
	s, out := imageSession(t, noModel(t), 0)
	s.deps.readConcurrency = 4
	client := &parallelPermissionClient{recorder: out, s: s, t: t}
	s.deps.out = client
	image := imageBlock(t, testImage(t, 2, 3))
	cfg := imagePeer(t, func(*http.Request) any { return map[string]any{"content": []any{image}} })
	if err := s.Resume(t.Context(), nil, []acp.MCPServer{cfg}, false); err != nil {
		t.Fatal(err)
	}
	round := s.request()
	binding := s.mcp.Snapshot()[0]
	binding.ReadOnly = true
	round.bindings[binding.Name] = binding
	calls := readCalls(2)
	calls = append(calls, llm.ToolCall{ID: "mcp", Function: llm.FunctionCall{Name: binding.Name, Arguments: `{}`}}, llm.ToolCall{ID: "after", Function: llm.FunctionCall{Name: "get_instructions", Arguments: `{"path":"."}`}})
	s.appendHistory(llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
	s.runTools(t.Context(), calls, round)
	assertToolPairs(t, s.history)
	if client.permissions != 1 || s.history[4].ToolCallID != "mcp" || len(s.history[4].Images) != 1 || s.history[5].ToolCallID != "after" {
		t.Fatal("MCP barrier lost permission, image or ordered result")
	}
}

func TestParallelReadStoppedTurnDoesNotStartSiblings(t *testing.T) {
	for _, reason := range []string{"cancel", "budget"} {
		t.Run(reason, func(t *testing.T) {
			s, out := budgetSession(t, noModel(t), 0, 0)
			s.deps.readConcurrency = 4
			calls := readCalls(6)
			calls = append(calls, llm.ToolCall{ID: "write", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}})
			s.appendHistory(llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := "Cancelled before it ran."
			if reason == "cancel" {
				cancel()
			} else {
				s.budget = &turnBudget{maxTokens: 1}
				s.budget.record(budgetUsage(1, 0))
				want = "budget reached"
			}
			s.runTools(ctx, calls, s.request())
			assertToolPairs(t, s.history)
			if out.permissions.Load() != 0 {
				t.Fatal("stopped turn asked permission")
			}
			for i, call := range calls {
				m := s.history[i+2]
				if m.ToolCallID != call.ID || !strings.Contains(m.Content, want) {
					t.Fatal("stopped turn lost matching result")
				}
			}
			for _, u := range s.transcript {
				if patch, ok := u.(acp.ToolCallUpdate); ok && patch.Status != acp.ToolFailed {
					t.Fatal("stopped turn started tool work")
				}
			}
		})
	}
}
