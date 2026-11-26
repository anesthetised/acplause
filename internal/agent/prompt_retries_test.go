package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

// keyedSaveGate holds the first keyed insertion immediately before or after
// the real commit, while other requests continue through the protocol reader.
type keyedSaveGate struct {
	Store
	commitFirst bool
	entered     chan context.Context
	release     chan struct{}
	held        bool // Save calls for the session are serialized by flushMu
}

func (g *keyedSaveGate) Save(ctx context.Context, sess store.Session, h, tr []store.Row) error {
	if g.held || len(tr) == 0 || tr[len(tr)-1].Key == "" {
		return g.Store.Save(ctx, sess, h, tr)
	}
	g.held = true
	if g.commitFirst {
		if err := g.Store.Save(ctx, sess, h, tr); err != nil {
			return err
		}
	}
	g.entered <- ctx
	<-g.release
	if g.commitFirst {
		return nil
	}
	return g.Store.Save(ctx, sess, h, tr)
}

func TestKeyedRetryCancellationAtCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[committed], func(t *testing.T) {
			gate := &keyedSaveGate{commitFirst: committed, entered: make(chan context.Context, 1), release: make(chan struct{})}
			model := make(scriptedModel, 1)
			model.reply("done", llm.FinishStop, nil)
			c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store { gate.Store = st; return gate })
			c.initialize()
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			first := c.promptWithKey(sid, "key", "first")
			unblock := sync.OnceFunc(func() { close(gate.release) })
			defer unblock()
			var saving context.Context
			select {
			case saving = <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("insertion did not reach store")
			}
			second := c.promptWithKey(sid, "key", "duplicate")
			c.notify("$/cancel_request", map[string]any{"requestId": c.lastID()})
			select {
			case <-saving.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("retry did not cancel the in-flight insertion")
			}
			unblock()
			one, two := c.await(first), c.await(second)
			retry := c.await(c.promptWithKey(sid, "key", "fresh attempt"))
			if retry.Error != nil {
				t.Fatal(retry.Error)
			}
			if committed {
				if one.Error != nil || two.Error != nil || string(one.Result) != string(retry.Result) || string(two.Result) != string(retry.Result) {
					t.Fatalf("commit: %+v %+v %+v", one, two, retry)
				}
			} else if one.Error == nil || one.Error.Code != -32800 || two.Error == nil || two.Error.Code != -32800 {
				t.Fatalf("withdrawal: %+v %+v", one, two)
			}
			updates := c.untilIdle(sid)
			var echoes, running int
			for _, n := range updates {
				if n.Update.Kind == "user_message" {
					echoes++
				}
				if n.Update.State == "running" {
					running++
				}
			}
			if echoes != 1 || running != 1 || lastIdle(updates).Update.StopReason != "end_turn" {
				t.Fatalf("turn lifecycle: %+v", updates)
			}
		})
	}
}

func TestKeyedSaveFallbackRetainsIdentityUntilFlush(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := newSession(ctx, deps{out: make(recorder, 16), log: slog.New(slog.DiscardHandler)}, newCatalog(nil), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := st.Create(ctx, s.record()); err != nil {
		t.Fatal(err)
	}
	s.deps.store = saveStore{save: func(context.Context, store.Session, []store.Row, []store.Row) error {
		return errors.New("ordinary save failure")
	}}
	sub := &submission{ctx: ctx, key: "key", prompt: []acp.ContentBlock{acp.Text("once")}, reply: jsonrpc.NewReply(ctx)}
	if !s.insert(ctx, sub) || !s.saveWarning {
		t.Fatal("missing in-memory insertion or warning")
	}
	result, rpcErr := settledReply(t, sub.reply)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	id := s.transcript[0].(acp.UserMessage).MessageID
	checkRetry := func(sess *Session) {
		t.Helper()
		retry := &submission{ctx: ctx, key: "key", reply: jsonrpc.NewReply(ctx)}
		sess.submit(retry)
		got, err := settledReply(t, retry.reply)
		var r acp.PromptResponse
		if err != nil || json.Unmarshal(got, &r) != nil || r.MessageID != id || string(got) != string(result) || len(sess.transcript) != 1 {
			t.Fatalf("retry: %s, %v", got, err)
		}
	}
	checkRetry(s)
	l, err := st.Acquire(ctx, s.id)
	if err != nil || len(l.Transcript) != 0 || len(l.TranscriptKeys) != 0 {
		t.Fatalf("partial save: %+v, %v", l, err)
	}
	s.deps.store = st
	s.flush()
	l, err = st.Acquire(ctx, s.id)
	if err != nil || len(l.Transcript) != 1 || l.TranscriptKeys[0] != "key" || len(l.History) != 1 {
		t.Fatalf("flush: %+v, %v", l, err)
	}
	resumed, err := restoreSession(ctx, deps{}, newCatalog(nil), l)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	checkRetry(resumed)
}

func TestKeyConflictRejectsInsertion(t *testing.T) {
	ctx := context.Background()
	s, _ := testSession(t, nil)
	s.saves.Wait()
	s.deps.store = saveStore{save: func(context.Context, store.Session, []store.Row, []store.Row) error { return store.ErrKeyConflict }}
	sub := &submission{ctx: ctx, key: "key", prompt: []acp.ContentBlock{acp.Text("conflict")}, reply: jsonrpc.NewReply(ctx)}
	if s.insert(ctx, sub) || s.saveWarning || len(s.keys) != 0 || len(s.transcript) != 0 || len(s.history) != 1 {
		t.Fatal("conflict admitted an in-memory duplicate")
	}
	_, err := settledReply(t, sub.reply)
	if err == nil || err.Code != -32603 {
		t.Fatalf("conflict: %+v", err)
	}
}

func TestKeyedAttemptsWhileQueued(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "accept", true: "withdraw"}[cancel], func(t *testing.T) {
			model := make(scriptedModel, 2)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			model <- func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &llm.Completion{Content: "first", FinishReason: llm.FinishStop}, nil
			}
			model.reply("done", llm.FinishStop, nil)
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			if r := c.await(c.prompt(sid, "block worker")); r.Error != nil {
				t.Fatal(r.Error)
			}
			one := c.promptWithKey(sid, "key", "queued")
			firstID := c.lastID()
			two := c.promptWithKey(sid, "key", "retry")
			// A subsequent response fences receipt while the worker is still busy.
			c.ok(acp.MethodSessionList, map[string]any{}, nil)
			if cancel {
				c.notify("$/cancel_request", map[string]any{"requestId": firstID})
				for _, ch := range []<-chan rpcResponse{one, two} {
					if r := c.await(ch); r.Error == nil || r.Error.Code != -32800 {
						t.Fatalf("withdrawal: %+v", r)
					}
				}
				one = c.promptWithKey(sid, "key", "new attempt")
				two = c.promptWithKey(sid, "key", "retry new attempt")
			}
			unblock()
			a, b := c.await(one), c.await(two)
			if a.Error != nil || b.Error != nil || string(a.Result) != string(b.Result) {
				t.Fatalf("queued results: %+v %+v", a, b)
			}
			c.untilIdle(sid)
			updates := c.untilIdle(sid)
			var echoes int
			for _, n := range updates {
				if n.Update.Kind == "user_message" {
					echoes++
				}
			}
			if echoes != 1 || lastIdle(updates).Update.StopReason != "end_turn" {
				t.Fatalf("queued turn: %+v", updates)
			}
		})
	}
}
