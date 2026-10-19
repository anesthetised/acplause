package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

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
	op := &sessionOperation{ctx: ctx, cancel: cancel, prompt: true, reply: jsonrpc.NewReply()}
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
