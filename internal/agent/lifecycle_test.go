package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

// settledReply reads the actual JSON-RPC result without exposing Reply internals.
func settledReply(t *testing.T, reply *jsonrpc.Reply) (json.RawMessage, *jsonrpc.Error) {
	t.Helper()
	var output bytes.Buffer
	conn := jsonrpc.NewConn(bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"test\"}\n"), &output, slog.New(slog.DiscardHandler))
	if err := conn.Serve(context.Background(), jsonrpc.Methods{"test": func(context.Context, json.RawMessage) (any, error) { return reply, nil }}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	var response struct {
		Result json.RawMessage
		Error  *jsonrpc.Error
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Result, response.Error
}

func TestCancelWaitingPromptPreservesCommittedInsertion(t *testing.T) {
	s, err := newSession(context.Background(), deps{out: make(recorder, 8)}, newCatalog([]llm.Model{{ID: "m"}}), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.model = "m"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	op := &sessionOperation{ctx: ctx, cancel: cancel, prompt: true, reply: jsonrpc.NewReply(context.Background())}
	a := &Agent{operations: map[string]*sessionOperation{s.id: {previous: op}}}
	s.deps.store = saveStore{save: func(context.Context, store.Session, []store.Row, []store.Row) error {
		a.cancelWaitingPrompts(s.id)
		return nil // The store committed even though cancellation raced its result.
	}}
	sub := &submission{ctx: ctx, prompt: []acp.ContentBlock{acp.Text("committed")}, reply: op.reply}
	if !s.insert(context.Background(), sub) {
		t.Fatal("committed prompt not inserted")
	}
	result, rpcErr := settledReply(t, op.reply)
	if rpcErr != nil || !bytes.Contains(result, []byte(`"messageId"`)) {
		t.Fatalf("committed prompt rejected: %s, %+v", result, rpcErr)
	}
	if len(s.history) != 2 {
		t.Fatal("committed prompt missing from history")
	}
}

type cancelledCreateStore struct {
	Store
	cancel           context.CancelFunc
	created, deleted bool
}

func (s *cancelledCreateStore) Selection(context.Context) (string, string, error) { return "", "", nil }
func (s *cancelledCreateStore) Create(context.Context, store.Session) error {
	s.created = true
	s.cancel()
	return nil
}
func (s *cancelledCreateStore) Delete(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.deleted = true
	return nil
}
func (s *cancelledCreateStore) Release(context.Context, string) error { return nil }

func TestCancelledNewSessionRemovesUnpublishedRecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &cancelledCreateStore{cancel: cancel}
	a := New(Config{Store: st}, make(recorder, 8), nil, func(context.Context) ([]llm.Model, error) { return []llm.Model{{ID: "m"}}, nil })
	defer a.Shutdown()
	value, err := a.newSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a.jobs.Wait()
	_, rpcErr := settledReply(t, value.(*jsonrpc.Reply))
	if rpcErr == nil || rpcErr.Code != -32800 {
		t.Fatalf("unexpected cancellation result: %+v", rpcErr)
	}
	if !st.created || !st.deleted {
		t.Fatalf("unpublished session survived: created=%v deleted=%v", st.created, st.deleted)
	}
}

func TestCloseRejectsOperationsThatCannotOpenSession(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		terminal, completed, prompt bool
	}{
		{name: "closing", terminal: true},
		{name: "prompt after close", terminal: true, prompt: true},
		{name: "completed setup", completed: true},
		{name: "prompt after completed setup", completed: true, prompt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			previous := &sessionOperation{ctx: ctx, cancel: cancel, terminal: tc.terminal, done: make(chan struct{})}
			if tc.completed {
				close(previous.done)
			}
			tail := previous
			if tc.prompt {
				tail = &sessionOperation{ctx: ctx, cancel: cancel, prompt: true, previous: previous, done: make(chan struct{})}
			}
			a := &Agent{ctx: context.Background(), operations: map[string]*sessionOperation{"closed": tail}}
			// Keep the operation in the map as response delivery races deferred
			// cleanup. Neither terminal cleanup nor a queued prompt can reopen it.
			defer func() {
				if !tc.completed {
					close(previous.done)
				}
				if tail != previous {
					close(tail.done)
				}
				a.jobs.Wait()
			}()
			_, err := a.closeSession(context.Background(), acp.CloseSessionRequest{SessionID: "closed"})
			if err == nil || jsonrpc.AsError(err).Code != -32002 {
				t.Fatalf("close of absent session accepted: %v", err)
			}
		})
	}
}

// blockingListStore holds List until released.
type blockingListStore struct {
	Store
	entered, release chan struct{}
}

func (s *blockingListStore) List(context.Context, string, int, int) ([]store.Session, bool, error) {
	close(s.entered)
	<-s.release
	return nil, false, nil
}

func TestShutdownWaitsForSessionList(t *testing.T) {
	st := &blockingListStore{entered: make(chan struct{}), release: make(chan struct{})}
	a := New(Config{Store: st}, make(recorder, 8), nil, func(context.Context) ([]llm.Model, error) { return nil, nil })
	if _, err := a.listSessions(context.Background(), acp.ListSessionsRequest{}); err != nil {
		t.Fatal(err)
	}
	<-st.entered
	done := make(chan struct{})
	go func() { a.Shutdown(); close(done) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while the store was still in use")
	case <-time.After(50 * time.Millisecond):
	}
	close(st.release)
	<-done
}

// cancelledAcquireStore cancels the request while loading and fails, as a
// store interrupted by its context does, without wrapping context.Canceled.
type cancelledAcquireStore struct {
	Store
	cancel context.CancelFunc
}

func (s *cancelledAcquireStore) Acquire(context.Context, string) (*store.Loaded, error) {
	s.cancel()
	return nil, errors.New("interrupted")
}

func TestCancelledSetupReportsCancellation(t *testing.T) {
	models := func(context.Context) ([]llm.Model, error) { return []llm.Model{{ID: "m"}}, nil }
	t.Run("resume", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a := New(Config{Store: &cancelledAcquireStore{cancel: cancel}}, make(recorder, 8), nil, models)
		defer a.Shutdown()
		value, err := a.resumeSession(ctx, acp.ResumeSessionRequest{SessionID: "s", Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		a.jobs.Wait()
		if _, rpcErr := settledReply(t, value.(*jsonrpc.Reply)); rpcErr == nil || rpcErr.Code != -32800 {
			t.Fatalf("got %+v, want a cancellation", rpcErr)
		}
	})
	t.Run("new session MCP", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			cancel() // the request ends while setup fails for another reason
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer peer.Close()
		a := New(Config{Store: &cancelledCreateStore{}}, make(recorder, 8), nil, models)
		defer a.Shutdown()
		value, err := a.newSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), MCPServers: []acp.MCPServer{{Type: "http", Name: "slow", URL: peer.URL}}})
		if err != nil {
			t.Fatal(err)
		}
		a.jobs.Wait()
		if _, rpcErr := settledReply(t, value.(*jsonrpc.Reply)); rpcErr == nil || rpcErr.Code != -32800 {
			t.Fatalf("got %+v, want a cancellation", rpcErr)
		}
	})
}
