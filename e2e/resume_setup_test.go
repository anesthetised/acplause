package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func TestCancelledColdResumePreservesDirectories(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer peer.Close()
	defer close(release)
	a := start(t, newMockLLM(t))
	a.initialize()
	cwd, oldDir, newDir := t.TempDir(), t.TempDir(), t.TempDir()
	sid := a.newSession(cwd, oldDir).SessionID
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
	resume := a.request("session/resume", map[string]any{
		"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{newDir},
		"mcpServers": []any{map[string]any{"type": "http", "name": "bad", "url": peer.URL}},
	})
	select {
	case <-entered:
	case <-time.After(timeout):
		t.Fatal("resume did not connect to MCP peer")
	}
	a.notify("$/cancel_request", map[string]any{"requestId": json.RawMessage(resume)})
	if r := a.response(resume); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("resume did not cancel: %+v", r)
	}
	var listed struct {
		Sessions []struct {
			SessionID             string
			AdditionalDirectories []string
		}
	}
	a.ok("session/list", map[string]any{}, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != sid || !slices.Equal(listed.Sessions[0].AdditionalDirectories, []string{oldDir}) {
		t.Fatalf("cancelled resume changed listed directories: %+v", listed)
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{newDir}}, nil)
	a.untilIdle(sid)
	a.ok("session/list", map[string]any{}, &listed)
	if len(listed.Sessions) != 1 || !slices.Equal(listed.Sessions[0].AdditionalDirectories, []string{newDir}) {
		t.Fatalf("retry did not apply directories: %+v", listed)
	}
}
