package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

func TestFailedColdResumePreservesDirectories(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer peer.Close()
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.initialize()
	cwd, oldDir, newDir := t.TempDir(), t.TempDir(), t.TempDir()
	var created acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, map[string]any{"cwd": cwd, "additionalDirectories": []string{oldDir}}, &created)
	c.ok(acp.MethodSessionClose, map[string]any{"sessionId": created.SessionID}, nil)
	c.fails(acp.MethodSessionResume, map[string]any{
		"sessionId": created.SessionID, "cwd": cwd, "additionalDirectories": []string{newDir},
		"mcpServers": []map[string]any{{"type": "http", "name": "bad", "url": peer.URL}},
	}, -32602)
	l, err := c.agent.cfg.Store.Acquire(context.Background(), created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Dirs, []string{oldDir}) {
		t.Fatalf("failed resume persisted directories: got %v; want %v", l.Dirs, []string{oldDir})
	}
	if l.AlreadyOwned {
		t.Fatal("failed resume retained its ownership lease")
	}
	if _, err := c.agent.session(created.SessionID); err == nil {
		t.Fatal("failed cold resume published a session")
	}
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": created.SessionID, "cwd": cwd, "additionalDirectories": []string{newDir}}, nil)
	c.untilIdle(created.SessionID)
	l, err = c.agent.cfg.Store.Acquire(context.Background(), created.SessionID)
	if err != nil || !slices.Equal(l.Dirs, []string{newDir}) {
		t.Fatalf("successful resume did not persist directories: %+v, %v", l, err)
	}
}

// slowReplayClient holds the first notification until release, standing in
// for a client that reads stdout slowly while the writer queue is full.
type slowReplayClient struct {
	recorder
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *slowReplayClient) Notify(method string, params any) error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.recorder.Notify(method, params)
}

func TestReplayDoesNotHoldTheSessionLock(t *testing.T) {
	s, out := testSession(t, nil)
	insertPlanPrompt(t, s)
	for len(out) > 0 {
		<-out
	}
	client := &slowReplayClient{recorder: out, entered: make(chan struct{}), release: make(chan struct{})}
	s.deps.out = client
	var release sync.Once
	unblock := func() { release.Do(func() { close(client.release) }) }
	defer unblock()
	resumed := make(chan error, 1)
	go func() { resumed <- s.Resume(context.Background(), nil, nil, true) }()
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not start the replay")
	}
	live := make(chan struct{})
	go func() {
		defer close(live)
		s.Cancel()
		s.setState(acp.StateUpdate{State: acp.StateIdle, StopReason: acp.StopCancelled})
	}()
	select {
	case <-live:
	case <-time.After(5 * time.Second):
		t.Fatal("session/cancel waited for the replay")
	}
	unblock()
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not finish")
	}
	if _, ok := (<-out).(acp.UserMessage); !ok {
		t.Fatal("replay lost the user message")
	}
	expectReplayComplete(t, out)
	if u := (<-out).(acp.StateUpdate); u.StopReason != "" {
		t.Fatalf("replay state: %+v", u)
	}
	if u := (<-out).(acp.StateUpdate); u.StopReason != acp.StopCancelled {
		t.Fatalf("live state overtook the replay: %+v", u)
	}
	if len(out) > 0 {
		t.Fatalf("unexpected update: %+v", <-out)
	}
}

func expectReplayComplete(t *testing.T, out recorder) {
	t.Helper()
	if u := <-out; u != (acp.ReplayComplete{}) {
		t.Fatalf("got %T, want replay_complete", u)
	}
}

// Keep the session lock across request receipt so the replay worker cannot
// take its cut yet. The protocol reader must still register the barrier.
func TestReplayFromReceiptThroughAcquire(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			gate := &gatedAcquireStore{entered: make(chan struct{}, 1), release: make(chan error, 1)}
			defer close(gate.release)
			c := startAgent(t, t.TempDir(), nil, nil, func(st Store) Store { gate.Store = st; return gate })
			c.initialize()
			cwd := t.TempDir()
			id := c.newSession(cwd)
			s, _ := c.agent.session(id)
			params := map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}}
			s.mu.Lock()
			unlock := sync.OnceFunc(s.mu.Unlock)
			defer unlock()
			resume := c.send(acp.MethodSessionResume, params)
			rpcID := c.lastID()
			c.ok(acp.MethodSessionList, map[string]any{}, nil) // reader has received resume
			msg := acp.UserMessage{MessageID: "at-cut", Content: []acp.ContentBlock{acp.Text("at cut")}}
			s.transcript = append(s.transcript, msg)
			s.send(msg)
			s.send(acp.Notice{Title: "before cut", Severity: "info"})
			unlock()
			issue50Wait(t, gate.entered)
			c.fails(acp.MethodSessionResume, params, 1001)
			s.mu.Lock()
			s.send(acp.Notice{Title: "during acquire", Severity: "info"})
			s.mu.Unlock()
			select {
			case n := <-c.updates:
				t.Fatalf("live update escaped before release: %+v", n)
			default:
			}
			want := []string{"user_message", "before cut", "during acquire"}
			switch outcome {
			case "success":
				gate.release <- nil
				want = []string{"user_message", "replay_complete", "before cut", "during acquire", "state_update"}
			case "failure":
				gate.release <- errors.New("acquire failed")
			case "cancel":
				c.notify("$/cancel_request", map[string]any{"requestId": rpcID})
				c.ok(acp.MethodSessionList, map[string]any{}, nil)
				gate.release <- nil
			}
			resp := c.await(resume)
			if (resp.Error == nil) != (outcome == "success") || (outcome == "cancel" && resp.Error.Code != -32800) {
				t.Fatalf("resume %s: %+v", outcome, resp)
			}
			c.agent.jobs.Wait() // failures release buffered live updates after their response
			c.ok(acp.MethodSessionList, map[string]any{}, nil)
			var got []string
			for len(c.updates) > 0 {
				u := (<-c.updates).Update
				if u.Kind == "notice" {
					got = append(got, u.Title)
				} else {
					got = append(got, u.Kind)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("updates = %v, want %v", got, want)
			}
		})
	}
}

type replayTestWriter func([]byte) (int, error)

func (w replayTestWriter) Write(line []byte) (int, error) { return w(line) }

// observedList fences receipt of earlier requests while stdout is blocked.
type observedList struct {
	Store
	entered chan struct{}
}

func (st observedList) List(ctx context.Context, cwd string, offset, limit int) ([]store.Session, bool, error) {
	close(st.entered)
	return st.Store.List(ctx, cwd, offset, limit)
}

func TestColdActivationAtResumeResponseWrite(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(map[bool]string{false: "prompt after response", true: "close before response"}[closing], func(t *testing.T) {
			entered, release, listed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			model := make(scriptedModel, 1)
			model.reply("done", llm.FinishStop, nil)
			c := startAgentWithOutput(t, t.TempDir(), model, nil, func(w io.Writer) io.Writer {
				// Hold the state snapshot just ahead of the queued response. The
				// marker already reached the client and the protocol reader is free.
				block := sync.OnceFunc(func() { close(entered); <-release })
				return replayTestWriter(func(line []byte) (int, error) {
					if bytes.Contains(line, []byte(`"sessionUpdate":"state_update"`)) {
						block()
					}
					return w.Write(line)
				})
			}, func(st Store) Store { return observedList{st, listed} })
			c.initialize()
			cwd := t.TempDir()
			id := c.newSession(cwd)
			c.selectModel(id, "m")
			c.ok(acp.MethodSessionClose, map[string]any{"sessionId": id}, nil)
			resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}})
			issue50Wait(t, entered)
			c.waitFor(id, func(n notification) bool { return n.Update.Kind == "replay_complete" })
			c.agent.jobs.Wait() // the successful response is queued
			c.agent.mu.Lock()
			generation := c.agent.sessions[id]
			c.agent.mu.Unlock()
			early := c.promptWithKey(id, "k", "too soon")
			c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": id})
			var closeReply <-chan rpcResponse
			if closing {
				closeReply = c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
			}
			c.send(acp.MethodSessionList, map[string]any{})
			issue50Wait(t, listed)
			c.agent.jobs.Wait()
			unblock()
			if r := c.await(resume); r.Error != nil {
				t.Fatal(r.Error)
			}
			// Act immediately on receipt. The stale success in the close case
			// must never reactivate its detached generation.
			if closing {
				if generation.activation.Load() != activationDetached {
					t.Fatal("queued success activated the closed generation")
				}
				c.sessionNotFound(acp.MethodSessionPrompt, map[string]any{"sessionId": id, "prompt": []any{}})
				if r := c.await(closeReply); r.Error != nil {
					t.Fatal(r.Error)
				}
			} else if r := c.await(c.promptWithKey(id, "k", "after success")); r.Error != nil {
				t.Fatal(r.Error)
			}
			if r := c.await(early); r.Error == nil || r.Error.Code != -32002 {
				t.Fatalf("prompt before response write: %+v", r)
			} else if data, ok := r.Error.Data.(map[string]any); !ok || data["sessionId"] != id {
				t.Fatalf("early prompt error data: %+v", r.Error.Data)
			}
		})
	}
}

func TestIncompleteReplayHasNoMarker(t *testing.T) {
	for _, failure := range []string{"peer write", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			s, out := testSession(t, nil)
			s.deps.log = slog.New(slog.DiscardHandler)
			insertPlanPrompt(t, s)
			for len(out) > 0 {
				<-out
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.deps.out = terminalPublicationClient{Client: out, publish: func(u acp.Update) error {
				if _, ok := u.(acp.UserMessage); ok {
					if failure == "peer write" {
						return errors.New("write failed")
					}
					cancel()
				}
				return nil
			}}
			if err := s.Resume(ctx, nil, nil, true); err == nil {
				t.Fatal("incomplete replay succeeded")
			}
			for len(out) > 0 {
				if u := <-out; u == (acp.ReplayComplete{}) {
					t.Fatal("incomplete replay sent a marker")
				}
			}
			s.deps.out = out
			if err := s.Resume(t.Context(), nil, nil, true); err != nil {
				t.Fatal(err)
			}
			if len(out) != 3 {
				t.Fatalf("retry sent %d updates, want history, marker and state", len(out))
			}
			<-out
			expectReplayComplete(t, out)
			if state, ok := (<-out).(acp.StateUpdate); !ok || state.State != acp.StateRunning {
				t.Fatalf("retry state: %+v", state)
			}
		})
	}
}

func TestCursorCannotHoldFinalUpdatesOwedByClose(t *testing.T) {
	entered := make(chan struct{})
	cancelled, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	model := fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	})
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	c.selectModel(id, "m")
	if r := c.await(c.prompt(id, "running")); r.Error != nil {
		t.Fatal(r.Error)
	}
	issue50Wait(t, entered)
	closing := c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
	issue50Wait(t, cancelled)
	params := map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}}
	c.fails(acp.MethodSessionResume, params, 1001)
	unblock()
	if r := c.await(closing); r.Error != nil {
		t.Fatal(r.Error)
	}
	if idle := lastIdle(c.untilIdle(id)); idle.Update.StopReason != "cancelled" {
		t.Fatalf("closing turn: %+v", idle)
	}
	c.ok(acp.MethodSessionResume, params, nil)
	if updates := c.untilIdle(id); !hasKind(updates, "replay_complete") {
		t.Fatalf("replay after close: %+v", updates)
	}
}

func TestCursorDuringLeaseRelease(t *testing.T) {
	gate := &gatedReleaseStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(gate.release) })
	defer unblock()
	c := startAgent(t, t.TempDir(), nil, nil, func(st Store) Store { gate.Store = st; return gate })
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	closing := c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
	issue50Wait(t, gate.entered)
	cursor := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}})
	c.ok(acp.MethodSessionList, map[string]any{}, nil)
	unblock()
	for _, ch := range []<-chan rpcResponse{closing, cursor} {
		if r := c.await(ch); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if !hasKind(c.untilIdle(id), "replay_complete") {
		t.Fatal("missing replay marker")
	}
}

func TestAdmissionsDuringColdResponseWrite(t *testing.T) {
	for _, outcome := range []string{"before bytes", "after bytes", "short", "failure", "close"} {
		t.Run(outcome, func(t *testing.T) {
			entered, release, listed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			armed := make(chan struct{})
			model := make(scriptedModel, 3)
			for range cap(model) {
				model.reply("ok", llm.FinishStop, nil)
			}
			c := startAgentWithOutput(t, t.TempDir(), model, nil, func(w io.Writer) io.Writer {
				return replayTestWriter(func(line []byte) (int, error) {
					select {
					case <-armed:
					default:
						return w.Write(line)
					}
					if !bytes.Contains(line, []byte(`"availableCommands"`)) || bytes.Contains(line, []byte(`"sessionId"`)) {
						return w.Write(line)
					}
					if outcome == "after bytes" {
						if _, err := w.Write(line); err != nil {
							return 0, err
						}
					}
					close(entered)
					<-release
					switch outcome {
					case "after bytes":
						return len(line), nil
					case "short":
						return 1, nil
					case "failure":
						return 0, io.ErrClosedPipe
					}
					return w.Write(line)
				})
			}, func(st Store) Store { return observedList{st, listed} })
			c.initialize()
			cwd := t.TempDir()
			id := c.newSession(cwd)
			other := c.newSession(cwd)
			c.ok(acp.MethodSessionClose, map[string]any{"sessionId": id}, nil)
			close(armed)
			resumed := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd})
			issue50Wait(t, entered)
			c.agent.mu.Lock()
			s := c.agent.sessions[id]
			c.agent.mu.Unlock()
			config := c.send(acp.MethodSessionSetConfig, map[string]any{"sessionId": id, "configId": configModel, "type": acp.ConfigValueID, "value": "r"})
			old := c.promptWithKey(id, "first", "withdrawn by session cancel")
			c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": id})
			old2 := c.promptWithKey(id, "first", "withdrawn by retry cancellation")
			retry := c.promptWithKey(id, "first", "retry")
			c.notify("$/cancel_request", map[string]any{"requestId": c.lastID()})
			first := c.promptWithKey(id, "first", "one")
			second := c.promptWithKey(id, "second", "two")
			joined := c.promptWithKey(id, "first", "duplicate")
			var newSession <-chan rpcResponse
			if outcome == "before bytes" || outcome == "after bytes" {
				// This later preference is visible to the next session even if the
				// staged choice takes longer to apply than the live-session choice.
				c.send(acp.MethodSessionSetConfig, map[string]any{"sessionId": other, "configId": configModel, "type": "id", "value": "m"})
				newSession = c.send(acp.MethodSessionNew, map[string]any{"cwd": cwd})
			}
			if outcome == "close" {
				c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
			}
			c.send(acp.MethodSessionList, map[string]any{})
			issue50Wait(t, listed)
			s.mu.Lock()
			if len(s.keys) != 0 || s.model != "" || len(model) != 3 {
				t.Errorf("work preceded write outcome: keys=%v model=%q", s.keys, s.model)
			}
			s.mu.Unlock()
			unblock()
			if outcome == "short" || outcome == "failure" {
				c.agent.jobs.Wait()
				s.mu.Lock()
				defer s.mu.Unlock()
				if len(s.keys) != 0 || s.model != "" || len(model) != 3 {
					t.Fatal("failed write admitted work")
				}
				return
			}
			if r := c.await(resumed); r.Error != nil {
				t.Fatal(r.Error)
			}
			if newSession != nil {
				var created acp.NewSessionResponse
				r := c.await(newSession)
				if r.Error != nil || json.Unmarshal(r.Result, &created) != nil {
					t.Fatalf("new session: %+v", r)
				}
				newlyCreated, err := c.agent.session(created.SessionID)
				if err != nil || newlyCreated.model != "m" {
					t.Fatalf("later session missed last model preference: %v", err)
				}
				c.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": id, "configId": configEffort, "type": "id", "value": "high"}, nil)
			}
			for _, ch := range []<-chan rpcResponse{old, old2, retry} {
				if r := c.await(ch); r.Error == nil || r.Error.Code != -32800 {
					t.Fatalf("withdrawn prompt = %+v", r)
				}
			}
			if outcome == "close" {
				for _, ch := range []<-chan rpcResponse{config, first, second, joined} {
					if r := c.await(ch); r.Error == nil {
						t.Fatal("close admitted staged work")
					}
				}
				return
			}
			third := c.promptWithKey(id, "third", "three")
			if r := c.await(config); r.Error != nil {
				t.Fatal(r.Error)
			}
			a, b, d := c.await(first), c.await(second), c.await(joined)
			if a.Error != nil || b.Error != nil || d.Error != nil || !bytes.Equal(a.Result, d.Result) || bytes.Equal(a.Result, b.Result) {
				t.Fatalf("keyed staging: %+v %+v %+v", a, b, d)
			}
			if r := c.await(third); r.Error != nil {
				t.Fatal(r.Error)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			var texts []string
			for _, u := range s.transcript {
				if m, ok := u.(acp.UserMessage); ok {
					texts = append(texts, m.Content[0].Text)
				}
			}
			if !slices.Equal(texts, []string{"one", "two", "three"}) {
				t.Fatalf("prompt order %v", texts)
			}
		})
	}
}

// The idle is on the wire but the turn still owns its final store write.
type pausedFinalSave struct {
	Store
	block            atomic.Bool
	entered, release chan struct{}
}

func (g *pausedFinalSave) Save(ctx context.Context, s store.Session, h, tr []store.Row, cancelled func() bool) error {
	if s.State == "idle" && g.block.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.Store.Save(ctx, s, h, tr, cancelled)
}

func TestCursorDuringClosingTurnsFinalSave(t *testing.T) {
	gate := &pausedFinalSave{entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(gate.release) })
	defer unblock()
	entered := make(chan struct{})
	model := fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store { gate.Store = st; return gate })
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	c.selectModel(id, "m")
	if r := c.await(c.prompt(id, "active")); r.Error != nil {
		t.Fatal(r.Error)
	}
	issue50Wait(t, entered)
	gate.block.Store(true)
	closing := c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
	issue50Wait(t, gate.entered)
	if idle := lastIdle(c.untilIdle(id)); idle.Update.StopReason != "cancelled" {
		t.Fatal(idle)
	}
	resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}})
	c.ok(acp.MethodSessionList, map[string]any{}, nil)
	unblock()
	for _, ch := range []<-chan rpcResponse{closing, resume} {
		if r := c.await(ch); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if !hasKind(c.untilIdle(id), "replay_complete") {
		t.Fatal("missing marker")
	}
}

func TestRetakeDrainsRetiringTurn(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		replay, closeDuring, renewedLoss bool
	}{
		{name: "cursorless"},
		{name: "replay", replay: true},
		{name: "close during replay", replay: true, closeDuring: true},
		{name: "renewal retiring worker", replay: true, renewedLoss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			model := fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				close(started)
				<-ctx.Done()
				close(cancelled)
				<-release
				return nil, ctx.Err()
			})
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			cwd := t.TempDir()
			id := c.newSession(cwd)
			c.selectModel(id, "m")
			if r := c.await(c.prompt(id, "running")); r.Error != nil {
				t.Fatal(r.Error)
			}
			issue50Wait(t, started)
			c.waitFor(id, func(n notification) bool { return n.Update.State == "running" })
			// The next acquisition observes discontinuous ownership even though
			// the old model request is still returning from cancellation.
			if err := c.agent.cfg.Store.Release(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			if tc.renewedLoss {
				c.agent.renew()
			}
			params := map[string]any{"sessionId": id, "cwd": cwd}
			if tc.replay {
				params["replayFrom"] = map[string]string{"type": "start"}
			}
			resume := c.send(acp.MethodSessionResume, params)
			issue50Wait(t, cancelled)
			var closing <-chan rpcResponse
			if tc.closeDuring {
				closing = c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
			}
			c.initialize() // The reader remains available during the drain.
			renewed := make(chan struct{})
			go func() { c.agent.renew(); close(renewed) }()
			issue50Wait(t, renewed)
			select {
			case r := <-resume:
				t.Fatalf("resume overtook retiring worker: %+v", r)
			case r := <-closing:
				t.Fatalf("close overtook retiring worker: %+v", r)
			case <-time.After(50 * time.Millisecond):
			}
			unblock()
			resp := c.await(resume)
			if tc.closeDuring {
				if resp.Error == nil || resp.Error.Code != -32800 {
					t.Fatalf("interrupted resume: %+v", resp)
				}
			} else if resp.Error != nil {
				t.Fatal(resp.Error)
			}
			if closing == nil {
				if err := c.agent.cfg.Store.CheckOwner(t.Context(), id); err != nil {
					t.Fatalf("replacement lease: %v", err)
				}
				closing = c.send(acp.MethodSessionClose, map[string]any{"sessionId": id})
			}
			if r := c.await(closing); r.Error != nil {
				t.Fatal(r.Error)
			}
			var got []string
			for len(c.updates) > 0 {
				u := (<-c.updates).Update
				if u.State != "" {
					got = append(got, strings.TrimSpace(u.State+" "+u.StopReason))
				} else {
					got = append(got, u.Kind)
				}
			}
			want := []string{"idle cancelled", "idle"}
			if tc.closeDuring {
				want = []string{"idle cancelled"}
			} else if tc.replay {
				want = []string{"user_message", "replay_complete", "idle cancelled", "idle"}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("updates before close = %q, want %q", got, want)
			}
		})
	}
}

func TestLostGenerationRejectsPromptDuringAcquire(t *testing.T) {
	gate := &gatedAcquireStore{entered: make(chan struct{}, 1), release: make(chan error)}
	defer close(gate.release)
	c := startAgent(t, t.TempDir(), nil, nil, func(st Store) Store { gate.Store = issue50Store{st}; return gate })
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	if err := c.agent.cfg.Store.Release(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s, _ := c.agent.session(id)
	s.flush() // A real failed owner check leaves the stale Session cached.
	if !s.isLost() {
		t.Fatal("stale save did not observe loss")
	}
	prompt := map[string]any{"sessionId": id, "prompt": []any{}, "idempotencyKey": "k"}
	if e := c.fails(acp.MethodSessionPrompt, prompt, -32603); e.Message != store.ErrLost.Error() {
		t.Fatal(e)
	}
	resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd})
	issue50Wait(t, gate.entered)
	c.sessionNotFound(acp.MethodSessionPrompt, prompt)
	gate.release <- nil
	if r := c.await(resume); r.Error != nil {
		t.Fatal(r.Error)
	}
}

func TestFailedReplayHistoryWriteDiscardsQueuedActivation(t *testing.T) {
	model := make(scriptedModel, 1)
	model.reply("recorded answer", llm.FinishStop, nil)
	var replaying atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	failure := errors.New("history frame write failed")
	var attempted [][]byte // read only after joining the connection writer
	c := startAgentWithOutput(t, t.TempDir(), model, nil, func(w io.Writer) io.Writer {
		return replayTestWriter(func(line []byte) (int, error) {
			if replaying.Load() {
				attempted = append(attempted, bytes.Clone(line))
				if bytes.Contains(line, []byte(`"sessionUpdate":"agent_message"`)) {
					close(entered)
					<-release
					return 0, failure
				}
			}
			return w.Write(line)
		})
	})
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	c.selectModel(id, "m")
	if r := c.await(c.prompt(id, "recorded prompt")); r.Error != nil {
		t.Fatal(r.Error)
	}
	c.untilIdle(id)
	c.ok(acp.MethodSessionClose, map[string]any{"sessionId": id}, nil)
	replaying.Store(true)
	c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}})
	issue50Wait(t, entered)
	c.agent.jobs.Wait() // Marker, state, and cold success are queued behind the failed frame.
	c.agent.mu.Lock()
	s := c.agent.sessions[id]
	c.agent.mu.Unlock()
	if s == nil || s.activation.Load() != activationPending {
		t.Fatal("expected pending activation behind replay frames")
	}
	unblock()
	if err := c.agent.deps.out.(*jsonrpc.Conn).Close(); !errors.Is(err, failure) {
		t.Fatalf("writer error = %v", err)
	}
	if len(attempted) != 2 || !bytes.Contains(attempted[0], []byte(`"sessionUpdate":"user_message"`)) ||
		!bytes.Contains(attempted[1], []byte(`"sessionUpdate":"agent_message"`)) {
		t.Fatalf("replay write attempts = %q", attempted)
	}
	if s.activation.Load() != activationPending {
		t.Fatal("discarded success activated the session")
	}
}
