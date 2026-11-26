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

func (g *keyedSaveGate) Save(ctx context.Context, sess store.Session, h, tr []store.Row, cancelled func() bool) error {
	if g.held || len(tr) == 0 || tr[len(tr)-1].Key == "" {
		return g.Store.Save(ctx, sess, h, tr, cancelled)
	}
	g.held = true
	if g.commitFirst {
		if err := g.Store.Save(ctx, sess, h, tr, cancelled); err != nil {
			return err
		}
	}
	g.entered <- ctx
	<-g.release
	if g.commitFirst {
		return nil
	}
	return g.Store.Save(ctx, sess, h, tr, cancelled)
}

func TestKeyedRetryCancellationAtCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		for _, member := range []string{"original", "duplicate"} {
			t.Run(map[bool]string{false: "before", true: "after"}[committed]+"/"+member, func(t *testing.T) {
				gate := &keyedSaveGate{commitFirst: committed, entered: make(chan context.Context, 1), release: make(chan struct{})}
				model := make(scriptedModel, 1)
				model.reply("done", llm.FinishStop, nil)
				c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store { gate.Store = st; return gate })
				c.initialize()
				sid := c.newSession(t.TempDir())
				c.selectModel(sid, "m")
				first := c.promptWithKey(sid, "key", "first")
				originalID := c.lastID()
				unblock := sync.OnceFunc(func() { close(gate.release) })
				defer unblock()
				var saving context.Context
				select {
				case saving = <-gate.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("insertion did not reach store")
				}
				second := c.promptWithKey(sid, "key", "duplicate")
				cancelID := originalID
				if member == "duplicate" {
					cancelID = c.lastID()
				}
				c.notify("$/cancel_request", map[string]any{"requestId": cancelID})
				c.initialize() // Fence the wire notification, without waiting on the save context.
				select {
				case <-saving.Done():
				default:
					t.Error("reader continued before the save was cancelled")
				}
				for _, response := range []<-chan rpcResponse{first, second} {
					select {
					case early := <-response:
						t.Fatalf("reply before the store returned: %+v", early)
					default:
					}
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
	s.deps.store = saveStore{Store: st, save: func(context.Context, store.Session, []store.Row, []store.Row) error {
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
		sess.submit(retry, nil)
		sess.saves.Wait()
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

func TestRecordedRetryDetectsOwnershipLoss(t *testing.T) {
	for _, operation := range []string{"takeover", "delete"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			model := make(scriptedModel, 1)
			model.reply("done", llm.FinishStop, nil)
			c := startAgent(t, dir, model, nil, func(st Store) Store { return issue50Store{st} })
			c.initialize()
			id := c.newSession(t.TempDir())
			c.selectModel(id, "m")
			if r := c.await(c.promptWithKey(id, "k", "first")); r.Error != nil {
				t.Fatal(r.Error)
			}
			c.untilIdle(id)
			s, _ := c.agent.session(id)
			issue50Flush(t, s)
			other, err := store.Open(dir, "other")
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if operation == "delete" {
				err = other.Delete(t.Context(), id)
			} else {
				_, err = other.Acquire(t.Context(), id)
			}
			if err != nil {
				t.Fatal(err)
			}
			r := c.await(c.promptWithKey(id, "k", "retry"))
			if r.Error == nil || r.Error.Code != -32603 || r.Error.Message != store.ErrLost.Error() || !s.isLost() {
				t.Fatalf("old owner retry = %+v, lost = %t", r, s.isLost())
			}
			if operation == "takeover" {
				if lost, err := other.Renew(t.Context(), []string{id}); err != nil || len(lost) != 0 {
					t.Fatalf("retry changed ownership: %v %v", lost, err)
				}
			}
		})
	}
}

type gatedReleaseStore struct {
	Store
	entered, release chan struct{}
}

func (g *gatedReleaseStore) Release(ctx context.Context, id string) error {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	return g.Store.Release(ctx, id)
}

func TestKeyedRetryAcrossCloseAndResume(t *testing.T) {
	gate := &gatedReleaseStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
	model := make(scriptedModel, 1)
	model.reply("done", llm.FinishStop, nil)
	c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store { gate.Store = st; return gate })
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	c.selectModel(id, "m")
	closing := c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
	unblock := sync.OnceFunc(func() { close(gate.release) })
	defer unblock()
	issue50Wait(t, gate.entered)
	closed := c.promptWithKey(id, "k", "while closed")
	resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd})
	for range 2 {
		c.sessionNotFound(acp.MethodSessionPrompt, map[string]any{"sessionId": id, "idempotencyKey": "k", "prompt": []any{}})
	}
	unblock()
	if r := c.await(closed); r.Error == nil || r.Error.Code != -32002 {
		t.Fatalf("closed retry = %+v", r)
	}
	for _, ch := range []<-chan rpcResponse{closing, resume} {
		if r := c.await(ch); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	one := c.promptWithKey(id, "k", "after activation")
	two := c.promptWithKey(id, "k", "duplicate")
	a, b := c.await(one), c.await(two)
	if a.Error != nil || b.Error != nil || string(a.Result) != string(b.Result) {
		t.Fatalf("resumed retries = %+v, %+v", a, b)
	}
}

// pausedCancellation exposes a cancelled waiter while holding its asynchronous
// callbacks. This makes the transaction-start check independent of scheduling.
type pausedCancellation struct {
	context.Context
	done chan struct{}
}

func (p pausedCancellation) Done() <-chan struct{} { return p.done }
func (p pausedCancellation) Err() error {
	select {
	case <-p.done:
		return context.Canceled
	default:
		return nil
	}
}
func (p pausedCancellation) AfterFunc(func()) func() bool { return func() bool { return true } }

func TestKeyedSaveChecksWaitersAtTransactionStart(t *testing.T) {
	for _, member := range []string{"original", "duplicate"} {
		t.Run(member, func(t *testing.T) {
			st, err := store.Open(t.TempDir(), "owner")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			gate := &keyedSaveGate{Store: st, entered: make(chan context.Context, 1), release: make(chan struct{})}
			s, err := newSession(t.Context(), deps{out: make(recorder, 16), store: gate}, newCatalog(nil), t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			unblock := sync.OnceFunc(func() { close(gate.release) })
			defer unblock()
			if err := st.Create(t.Context(), s.record()); err != nil {
				t.Fatal(err)
			}
			var p pendingPrompts
			id := promptIdentity{s.id, "key"}
			waiter := pausedCancellation{t.Context(), make(chan struct{})}
			original, duplicate := context.Context(waiter), t.Context()
			if member == "duplicate" {
				original, duplicate = duplicate, original
			}
			first := jsonrpc.NewReply(original)
			ctx, group, _ := p.join(original, id, first)
			sub := &submission{ctx: ctx, reply: group, key: id.key, prompt: []acp.ContentBlock{acp.Text("first")}}
			inserted := make(chan bool, 1)
			go func() { inserted <- s.insert(t.Context(), sub) }()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("save did not reach the transaction")
			}
			second := jsonrpc.NewReply(duplicate)
			p.join(duplicate, id, second)
			close(waiter.done)
			unblock()
			if <-inserted {
				t.Fatal("transaction admitted a cancelled waiter")
			}
			for _, r := range []*jsonrpc.Reply{first, second} {
				if _, err := settledReply(t, r); err == nil || err.Code != -32800 {
					t.Fatalf("waiter error = %v", err)
				}
			}
			l, err := st.Acquire(t.Context(), s.id)
			if err != nil || len(l.Transcript) != 0 || len(l.History) != 0 || l.State != "idle" {
				t.Fatalf("rejected insertion reached SQLite: %+v, %v", l, err)
			}
		})
	}
}

// gatedOwnerCheck lets the reader handle close while a cache hit is checking
// SQLite, and lets the test distinguish an I/O error from confirmed loss.
type gatedOwnerCheck struct {
	Store
	entered chan struct{}
	release chan error
}

func (g *gatedOwnerCheck) CheckOwner(ctx context.Context, id string) error {
	checked := g.Store.CheckOwner(ctx, id)
	g.entered <- struct{}{}
	if err := <-g.release; err != nil {
		return err
	}
	return checked
}

func TestRecordedRetryCheckAndClose(t *testing.T) {
	gate := &gatedOwnerCheck{entered: make(chan struct{}, 1), release: make(chan error, 1)}
	model := make(scriptedModel, 1)
	model.reply("done", llm.FinishStop, nil)
	c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store { gate.Store = st; return gate })
	defer close(gate.release)
	c.initialize()
	id := c.newSession(t.TempDir())
	c.selectModel(id, "m")
	original := c.await(c.promptWithKey(id, "k", "original"))
	if original.Error != nil {
		t.Fatal(original.Error)
	}
	c.untilIdle(id)

	first := c.promptWithKey(id, "k", "retry")
	issue50Wait(t, gate.entered)
	gate.release <- errors.New("read unavailable")
	if r := c.await(first); r.Error == nil || r.Error.Code != -32603 || r.Error.Message == store.ErrLost.Error() {
		t.Fatalf("unconfirmed ownership = %+v", r)
	}
	retry := c.promptWithKey(id, "k", "retry again")
	issue50Wait(t, gate.entered)
	closing := c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
	c.initialize() // The reader passed close with the ownership query still pending.
	select {
	case r := <-closing:
		t.Fatalf("close passed an admitted lookup: %+v", r)
	default:
	}
	gate.release <- nil
	if r := c.await(retry); r.Error != nil || string(r.Result) != string(original.Result) {
		t.Fatalf("admitted retry = %+v, want %+v", r, original)
	}
	if r := c.await(closing); r.Error != nil {
		t.Fatal(r.Error)
	}
}

func TestRecordedRetryArrivingAfterOwnershipRead(t *testing.T) {
	dir := t.TempDir()
	gate := &gatedOwnerCheck{entered: make(chan struct{}, 1), release: make(chan error)}
	model := make(scriptedModel, 1)
	model.reply("done", llm.FinishStop, nil)
	c := startAgent(t, dir, model, nil, func(st Store) Store { gate.Store = issue50Store{st}; return gate })
	unblock := sync.OnceFunc(func() { close(gate.release) })
	defer unblock()
	c.initialize()
	id := c.newSession(t.TempDir())
	c.selectModel(id, "m")
	original := c.await(c.promptWithKey(id, "k", "original"))
	c.untilIdle(id)
	s, _ := c.agent.session(id)
	issue50Flush(t, s)
	first := c.promptWithKey(id, "k", "while owned")
	issue50Wait(t, gate.entered) // SQLite has already returned its answer.
	other, err := store.Open(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Acquire(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	second := c.promptWithKey(id, "k", "after takeover")
	c.initialize()
	unblock()
	if r := c.await(first); r.Error != nil || string(r.Result) != string(original.Result) {
		t.Fatalf("first retry = %+v, want %+v", r, original)
	}
	if r := c.await(second); r.Error == nil || r.Error.Message != store.ErrLost.Error() {
		t.Fatalf("retry used an ownership read from before it arrived: %+v", r)
	}
}

func TestRetainedRetryAfterFailedSetup(t *testing.T) {
	model := make(scriptedModel, 1)
	model.reply("done", llm.FinishStop, nil)
	gate := &gatedAcquireStore{entered: make(chan struct{}, 1), release: make(chan error, 1)}
	defer close(gate.release)
	owner := &gatedOwnerCheck{entered: make(chan struct{}, 1), release: make(chan error, 1)}
	defer close(owner.release)
	c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store {
		owner.Store = st
		gate.Store = owner
		return gate
	})
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	c.selectModel(id, "m")
	original := c.await(c.promptWithKey(id, "k", "first"))
	if original.Error != nil {
		t.Fatal(original.Error)
	}
	c.untilIdle(id)
	resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "additionalDirectories": []string{cwd + "/absent"}})
	issue50Wait(t, gate.entered)
	retry := c.promptWithKey(id, "k", "retry")
	newKey := c.promptWithKey(id, "new", "new work")
	c.initialize()
	gate.release <- nil
	if r := c.await(resume); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("setup: %+v", r)
	}
	issue50Wait(t, owner.entered)
	owner.release <- nil
	if r := c.await(retry); r.Error != nil || string(r.Result) != string(original.Result) {
		t.Fatalf("retained retry = %+v, want %+v", r, original)
	}
	if r := c.await(newKey); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("new key: %+v", r)
	}
}
