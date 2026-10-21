package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

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
