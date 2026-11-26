package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUnsupportedSystemPromptOnNewAndColdResume(t *testing.T) {
	t.Parallel()
	var mcpRequests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mcpRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(peer.Close)
	a := start(t, newMockLLM(t))
	var initialized struct {
		Capabilities struct{ Session map[string]json.RawMessage }
	}
	a.ok("initialize", map[string]any{"protocolVersion": 2, "info": map[string]string{"name": "e2e", "version": "1"}}, &initialized)
	if _, advertised := initialized.Capabilities.Session["systemPrompt"]; advertised {
		t.Fatal("unsupported systemPrompt capability advertised")
	}
	cwd, oldDir, newDir := t.TempDir(), t.TempDir(), t.TempDir()
	servers := []any{map[string]any{"type": "http", "name": "unused", "url": peer.URL}}
	const marker = "private-client-system-prompt-test-marker"
	text := []any{map[string]any{"type": "text", "text": marker}}
	assertRejected := func(method string, params map[string]any) {
		t.Helper()
		err := a.fails(method, params, -32602)
		if !strings.Contains(err.Message, "systemPrompt") || strings.Contains(err.Message, marker) || bytes.Contains(err.Data, []byte(marker)) {
			t.Fatal("expected a systemPrompt error without prompt content")
		}
	}
	list := func() json.RawMessage {
		t.Helper()
		return a.ok("session/list", map[string]any{}, nil).Result
	}
	empty := list()
	assertRejected("session/new", map[string]any{
		"cwd": cwd, "mcpServers": servers, "systemPrompt": map[string]any{"mode": "append", "content": text},
	})
	if !bytes.Equal(list(), empty) {
		t.Fatal("rejected session/new created a session")
	}

	var created newSessionResult
	a.ok("session/new", map[string]any{"cwd": cwd, "additionalDirectories": []string{oldDir}, "systemPrompt": nil}, &created)
	sid := created.SessionID
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
	before := list()
	assertRejected("session/resume", map[string]any{
		"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{newDir}, "mcpServers": servers,
		"replayFrom": map[string]string{"type": "start"}, "systemPrompt": map[string]any{"mode": "override", "content": text},
	})
	a.sessionNotFound("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"})
	if !bytes.Equal(list(), before) || mcpRequests.Load() != 0 {
		t.Fatal("rejected setup changed session state or contacted an MCP peer")
	}

	resume := a.ok("session/resume", map[string]any{
		"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{oldDir},
		"replayFrom": map[string]string{"type": "start"}, "systemPrompt": nil,
	}, nil)
	updates := a.untilIdle(sid)
	if got := kinds(updates); !slices.Equal(got, []string{"replay_complete", "session_info_update", "state_update idle"}) || updates[len(updates)-1].seq >= resume.seq {
		t.Fatalf("cold replay with null systemPrompt: %v", got)
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, nil)
	if got := kinds(a.untilIdle(sid)); !slices.Equal(got, []string{"session_info_update", "state_update idle"}) {
		t.Fatalf("resume with omitted systemPrompt: %v", got)
	}
	a.stop()
	if bytes.Contains(a.stderr.Bytes(), []byte(marker)) {
		t.Fatal("rejected systemPrompt content entered the logs")
	}
}

func TestUnsupportedSystemPromptOnActiveReplayPreservesMCPAndRetry(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "alpha", "read_file", `{}`), reply("done"))
	a := start(t, llm, askingUser)
	a.initialize()
	cwd := t.TempDir()
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", filepath.Join(cwd, "calls"), "original"))
	prompt := func(text string) string {
		return a.request("session/prompt", map[string]any{
			"sessionId": sid, "idempotencyKey": "original-key", "prompt": []any{map[string]any{"type": "text", "text": text}},
		})
	}
	original, _ := a.accepted(prompt("call the tool"))
	permission := a.permissionRequest()
	a.fails("session/resume", map[string]any{
		"sessionId": sid, "cwd": cwd, "replayFrom": map[string]string{"type": "start"},
		"systemPrompt": map[string]any{"mode": "append", "content": []any{promptImage(t, "image/png")}},
	}, -32602)
	if got, _ := a.accepted(prompt("retry with the same key")); got != original {
		t.Fatalf("retained retry = %q, want %q", got, original)
	}
	a.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
	updates := a.untilIdle(sid)
	if len(filter(updates, "replay_complete")) != 0 || len(filter(updates, "user_message")) != 1 || updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("rejected resume disturbed the live turn: %v", kinds(updates))
	}
	messages := llm.request(1).Messages
	result, _ := messages[len(messages)-1]["content"].(string)
	if !strings.Contains(result, `"label":"original"`) || llm.requestCount() != 2 {
		t.Fatal("original MCP connection or prompt retry outcome changed")
	}
}
