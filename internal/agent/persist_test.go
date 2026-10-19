package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

func TestRestoreRepairsInterruptedTools(t *testing.T) {
	for _, status := range []acp.ToolCallStatus{acp.ToolPending, acp.ToolInProgress} {
		t.Run(string(status), func(t *testing.T) {
			assistant, err := json.Marshal(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "done"}, {ID: "unfinished"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			answered, err := json.Marshal(toolResult("done", "already finished"))
			if err != nil {
				t.Fatal(err)
			}
			call, err := encodeEntry(acp.ToolCallUpdate{ToolCallID: "unfinished", Name: "bash", Status: status})
			if err != nil {
				t.Fatal(err)
			}
			s, err := restoreSession(context.Background(), deps{}, newCatalog(nil), &store.Loaded{
				Session: store.Session{ID: "restored", Cwd: t.TempDir(), State: "running"},
				History: [][]byte{assistant, answered}, Transcript: [][]byte{call},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.state.State != acp.StateIdle || len(s.history) != 4 {
				t.Fatalf("recovered state=%v, history=%+v", s.state, s.history)
			}
			if got := s.history[3]; got.Role != llm.RoleTool || got.ToolCallID != "unfinished" || got.Content != interrupted {
				t.Fatalf("recovered result: %+v", got)
			}
			got := s.transcript[0].(acp.ToolCallUpdate)
			if got.Status != acp.ToolFailed || got.Name != "bash" || string(got.RawOutput) != string(jsonString(interrupted)) {
				t.Fatalf("recovered replay: %+v", got)
			}
			_, history, transcript, _, err := s.pending()
			if err != nil || len(history) != 1 || len(transcript) != 1 {
				t.Fatalf("repair not queued for persistence: %v, %v, %v", history, transcript, err)
			}
			s.repair()
			if len(s.history) != 4 {
				t.Fatal("repair duplicated a tool result")
			}
		})
	}
}

// gatedStore holds prompt persistence at the ownership check.
type gatedStore struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (g *gatedStore) Save(ctx context.Context, sess store.Session, _, _ []store.Row) error {
	if sess.State == string(acp.StateRunning) {
		close(g.entered)
		<-g.release
		return ctx.Err()
	}
	return nil
}

func TestPromptSaveDoesNotBlockSessionHandlers(t *testing.T) {
	g := &gatedStore{entered: make(chan struct{}), release: make(chan struct{})}
	out := make(recorder, 32)
	s, err := newSession(context.Background(), deps{out: out, store: g}, newCatalog([]llm.Model{{ID: "m"}}), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer close(g.release)
	s.model = "m"
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("not inserted yet")})
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not reach the store")
	}

	done := make(chan error, 1)
	go func() {
		if err := s.SetConfigOption(configModel, acp.ConfigValueID, json.RawMessage(`"m"`)); err != nil {
			done <- err
			return
		}
		if err := s.Resume(context.Background(), nil, nil, true); err != nil {
			done <- err
			return
		}
		s.Cancel()
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("session handlers blocked on prompt persistence")
	}
	if u := <-out; u.(acp.StateUpdate).State != acp.StateIdle {
		t.Fatalf("uncommitted prompt visible in replay: %+v", u)
	}
	select {
	case u := <-out:
		t.Fatalf("unexpected update before insertion: %+v", u)
	default:
	}
}

// saveStore isolates the transaction outcome at the insertion boundary.
type saveStore struct {
	Store
	save func(context.Context, store.Session, []store.Row, []store.Row) error
}

func TestPromptOwnershipLossRevokesMCPImmediately(t *testing.T) {
	s, err := newSession(context.Background(), deps{out: make(recorder, 8)}, newCatalog(nil), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.deps.store = saveStore{save: func(context.Context, store.Session, []store.Row, []store.Row) error { return store.ErrLost }}
	sub := &submission{ctx: context.Background(), prompt: []acp.ContentBlock{acp.Text("lost")}, reply: jsonrpc.NewReply()}
	if s.insert(context.Background(), sub) {
		t.Fatal("lost prompt was inserted")
	}
	if s.ctx.Err() == nil {
		t.Fatal("ownership loss left session and MCP lifetime alive")
	}
}

// A background save can find the session lost after its prompt was queued.
// Losing ownership also cancels the turn, but the client must learn why.
func TestQueuedPromptReportsLossNotCancellation(t *testing.T) {
	s, err := newSession(context.Background(), deps{out: make(recorder, 8)}, newCatalog(nil), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	turn, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.markLost()
	sub := &submission{ctx: context.Background(), prompt: []acp.ContentBlock{acp.Text("mine?")}, reply: jsonrpc.NewReply()}
	if s.insert(turn, sub) {
		t.Fatal("lost prompt was inserted")
	}
	if _, rpcErr := settledReply(t, sub.reply); rpcErr == nil || rpcErr.Message != store.ErrLost.Error() {
		t.Fatalf("lost prompt rejected with %+v", rpcErr)
	}
}

func (s saveStore) Save(ctx context.Context, sess store.Session, h, t []store.Row) error {
	return s.save(ctx, sess, h, t)
}

func TestPromptInsertionAtCommitBoundary(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := make(recorder, 32)
			s, err := newSession(context.Background(), deps{out: out}, newCatalog(nil), t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.deps.store = saveStore{save: func(_ context.Context, sess store.Session, h, tr []store.Row) error {
				if sess.State != "running" || len(h) != 1 || len(tr) != 1 {
					t.Fatalf("prompt not staged with running state: %+v, %v, %v", sess, h, tr)
				}
				cancel()
				if !committed {
					return context.Canceled
				}
				return nil
			}}
			sub := &submission{ctx: context.Background(), prompt: []acp.ContentBlock{acp.Text("hello")}, reply: jsonrpc.NewReply()}
			if inserted := s.insert(ctx, sub); inserted != committed {
				t.Fatalf("inserted=%t, committed=%t", inserted, committed)
			}
			if committed {
				if len(s.history) != 2 || len(s.transcript) != 1 || s.saved != 1 || s.state.State != acp.StateRunning {
					t.Fatal("committed prompt missing from memory")
				}
				if _, ok := (<-out).(acp.UserMessage); !ok {
					t.Fatal("committed prompt not echoed")
				}
			} else if len(s.history) != 1 || len(s.transcript) != 0 || len(out) != 0 || s.title != "" {
				t.Fatal("rolled-back prompt escaped into the session")
			}
		})
	}
}

func TestRestoreRepairsInterruptedCompaction(t *testing.T) {
	data, err := encodeEntry(acp.CompactionUpdate{CompactionID: "c", Status: acp.CompactionInProgress})
	if err != nil {
		t.Fatal(err)
	}
	s, err := restoreSession(context.Background(), deps{}, newCatalog(nil), &store.Loaded{
		Session: store.Session{ID: "s", Cwd: t.TempDir(), State: "running"},
		History: [][]byte{[]byte(`{"role":"user","content":"keep this"}`)}, Transcript: [][]byte{data},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.transcript[0].(acp.CompactionUpdate)
	if c.Status != acp.CompactionFailed || c.Error == "" || c.CompactionID != "c" || len(s.history) != 2 || s.history[1].Content != "keep this" {
		t.Fatalf("interrupted compaction: %+v, %+v", c, s.history)
	}
	_, history, transcript, _, err := s.pending()
	if err != nil || len(history) != 0 || len(transcript) != 1 {
		t.Fatalf("repair was not queued: %v, %v, %v", history, transcript, err)
	}
}

func TestSessionModeRecordAndRecovery(t *testing.T) {
	for _, mode := range []string{"", "build", "plan"} {
		t.Run(mode, func(t *testing.T) {
			s, err := restoreSession(context.Background(), deps{}, newCatalog(nil), &store.Loaded{Session: store.Session{ID: "s", Cwd: t.TempDir(), State: "idle", Mode: mode}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			want := mode
			if want == "" {
				want = "build"
			}
			if s.mode != want || s.record().Mode != want {
				t.Fatalf("mode not recovered: %q %+v", s.mode, s.record())
			}
		})
	}
	s, _ := testSession(t, nil)
	if s.record().Mode != "build" {
		t.Fatalf("new mode: %q", s.record().Mode)
	}
}

func TestCloseWaitsForModePersistence(t *testing.T) {
	entered, release, saved := make(chan struct{}), make(chan struct{}), make(chan string, 1)
	st := saveStore{save: func(_ context.Context, sess store.Session, _, _ []store.Row) error {
		close(entered)
		<-release
		saved <- sess.Mode
		return nil
	}}
	s, err := newSession(context.Background(), deps{store: st}, newCatalog(nil), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfigOption("mode", "id", jsonString("plan")); err != nil {
		t.Fatal(err)
	}
	<-entered
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
		t.Error("Close returned while mode save was still in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done
	if mode := <-saved; mode != "plan" {
		t.Fatalf("saved mode=%q", mode)
	}
}
