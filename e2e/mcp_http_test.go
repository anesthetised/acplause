package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPHTTPRelaySurvivesFailureAndResume(t *testing.T) {
	t.Parallel()
	var sessions atomic.Int32
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture") != "configured" {
			t.Error("client headers lost")
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("s-%d", sessions.Add(1)))
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "schedule_task", "description": "fixture:schedule_task", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			calls.Add(1)
			if strings.Contains(string(req.Params), `"fail":true`) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if r.Header.Get("Mcp-Session-Id") == "" || r.Header.Get("MCP-Protocol-Version") != "2025-11-25" {
				t.Error("missing MCP headers")
			}
			var p struct{ Arguments json.RawMessage }
			_ = json.Unmarshal(req.Params, &p)
			result = map[string]any{"content": []any{}, "structuredContent": p.Arguments}
		}
		message := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		if req.Method == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(message)
			fmt.Fprintf(w, "data: %s\n\n", b)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(message)
		}
	}))
	defer srv.Close()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "http", "schedule_task", `{"fail":true}`), mcpCall(t, "http", "schedule_task", `{"schedule":{"type":"interval","everyMs":3600000}}`), reply("done"), mcpCall(t, "http", "schedule_task", `{"taskId":"opaque"}`), reply("done"))
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	cfg := map[string]any{"type": "http", "name": "http", "url": srv.URL, "headers": []any{map[string]string{"name": "X-Fixture", "value": "configured"}}}
	sid := mcpSession(t, a, cwd, cfg)
	a.accepted(a.prompt(sid, "schedule"))
	updates := a.untilIdle(sid)
	failures, completed := 0, 0
	for _, u := range updates {
		if u.Status == "failed" {
			failures++
		}
		if u.Status == "completed" {
			completed++
		}
	}
	if failures != 1 || completed != 1 {
		t.Fatalf("wrong HTTP tool lifecycle: %v", kinds(updates))
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{cfg}, "replayFrom": map[string]string{"type": "start"}}, nil)
	// Discard the resume state before consuming a new turn's idle.
	a.updateWhere(sid, func(u update) bool { return u.is("state_update") && u.State == "idle" })
	a.accepted(a.prompt(sid, "use reference"))
	a.untilIdle(sid)
	msgs := llm.request(2).Messages
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), `"schedule":{"type":"interval","everyMs":3600000}`) {
		t.Fatal("HTTP nested args lost")
	}
	a.ok("session/close", map[string]string{"sessionId": sid}, nil)
	if sessions.Load() != 1 || calls.Load() != 3 {
		t.Fatalf("unchanged resume reconnected: sessions=%d calls=%d", sessions.Load(), calls.Load())
	}
}
