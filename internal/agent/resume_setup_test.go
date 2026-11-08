package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
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
