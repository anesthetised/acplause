package e2e

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestFailedColdResumePreservesDirectories(t *testing.T) {
	t.Parallel()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer peer.Close()
	a := start(t, newMockLLM(t))
	a.initialize()
	cwd, oldDir, newDir := t.TempDir(), t.TempDir(), t.TempDir()
	sid := a.newSession(cwd, oldDir).SessionID
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
	a.fails("session/resume", map[string]any{
		"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{newDir},
		"mcpServers": []any{map[string]any{"type": "http", "name": "bad", "url": peer.URL}},
	}, -32602)
	var listed struct {
		Sessions []struct {
			SessionID             string
			AdditionalDirectories []string
		}
	}
	a.ok("session/list", map[string]any{}, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != sid || !slices.Equal(listed.Sessions[0].AdditionalDirectories, []string{oldDir}) {
		t.Fatalf("failed resume changed listed directories: %+v", listed)
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{newDir}}, nil)
	a.untilIdle(sid)
	a.ok("session/list", map[string]any{}, &listed)
	if len(listed.Sessions) != 1 || !slices.Equal(listed.Sessions[0].AdditionalDirectories, []string{newDir}) {
		t.Fatalf("retry did not apply directories: %+v", listed)
	}
}
