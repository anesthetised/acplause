package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// issue55Peer holds a real HTTP call while list-changed refreshes the same peer.
// The second refresh request proves the first catalog publication has finished.
type issue55Peer struct {
	url                                   string
	change, entered, published, cancelled chan struct{}
	release                               chan struct{}
	calls                                 atomic.Int32
}

func newIssue55Peer(t *testing.T, remove bool) *issue55Peer {
	t.Helper()
	p := &issue55Peer{change: make(chan struct{}), entered: make(chan struct{}), published: make(chan struct{}), cancelled: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	listed := make(chan struct{})
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for _, gate := range []<-chan struct{}{p.change, listed} {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
				w.(http.Flusher).Flush()
			}
			<-r.Context().Done()
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			ID     json.RawMessage
			Method string
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
		var list int32
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			list = lists.Add(1)
			tools := []any{}
			if list == 1 || !remove {
				description := "fixture:effect"
				if list > 1 {
					description = "changed"
				}
				tools = append(tools, map[string]any{"name": "effect", "description": description, "inputSchema": map[string]string{"type": "object"}})
			}
			result = map[string]any{"tools": tools}
			if list == 3 {
				close(p.published)
			}
		case "tools/call":
			if p.calls.Add(1) == 1 {
				close(p.entered)
			}
			select {
			case <-p.release:
			case <-r.Context().Done():
				select {
				case p.cancelled <- struct{}{}:
				default:
				}
				return
			}
			result = map[string]any{"content": []any{}, "structuredContent": map[string]string{"receipt": "original-effect"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if list == 2 {
			close(listed)
		}
	}))
	p.url = srv.URL
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(p.release) })
	return p
}

func issue55Wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for " + what)
	}
}

func TestIssue55MCPRefreshAtPermissionAndAfterDispatch(t *testing.T) {
	for _, change := range []string{"description", "removal"} {
		for _, phase := range []string{"permission", "dispatched"} {
			t.Run(change+"/"+phase, func(t *testing.T) {
				t.Parallel()
				peer := newIssue55Peer(t, change == "removal")
				llm := newMockLLM(t)
				llm.enqueue(mcpCall(t, "fixture", "effect", `{}`), reply("done"))
				a := start(t, llm, askingUser)
				a.initialize()
				sid := mcpSession(t, a, t.TempDir(), map[string]any{"type": "http", "name": "fixture", "url": peer.url, "headers": []any{}})
				a.accepted(a.prompt(sid, "effect"))
				permission := a.permissionRequest()
				if phase == "dispatched" {
					a.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
					issue55Wait(t, peer.entered, "accepted tools/call")
				}
				close(peer.change)
				issue55Wait(t, peer.published, "catalog publication")
				if phase == "dispatched" {
					select {
					case <-peer.cancelled:
						t.Error("catalog refresh cancelled accepted HTTP call before response release")
					case <-time.After(100 * time.Millisecond):
					}
					peer.release <- struct{}{}
				}
				updates := a.untilIdle(sid)
				wantStatus, wantCalls := "completed", int32(1)
				if phase == "permission" {
					wantStatus, wantCalls = "failed", 0
				}
				found := false
				for _, u := range updates {
					if u.is("tool_call_update") && u.Status == wantStatus {
						found = true
						if phase == "dispatched" && !strings.Contains(u.text(), "original-effect") {
							t.Error("ACP completion lost original receipt")
						}
					}
				}
				if !found || peer.calls.Load() != wantCalls || updates[len(updates)-1].StopReason != "end_turn" {
					t.Errorf("want tool status=%s calls=%d; calls=%d updates=%v", wantStatus, wantCalls, peer.calls.Load(), kinds(updates))
				}
				msgs := llm.request(1).Messages
				last := msgs[len(msgs)-1]
				if last["role"] != "tool" || last["tool_call_id"] != "mcp-call-effect" || (phase == "dispatched" && !strings.Contains(last["content"].(string), "original-effect")) {
					t.Errorf("original result lost from model history: %+v", last)
				}
				if phase == "permission" {
					a.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
					// A following completed turn fences processing of the late answer.
					llm.enqueue(reply("late approval ignored"))
					a.accepted(a.prompt(sid, "continue"))
					a.untilIdle(sid)
					if peer.calls.Load() != 0 {
						t.Fatal("late approval dispatched an obsolete call")
					}
				}
			})
		}
	}
}

func TestIssue55MCPAdmittedCancellation(t *testing.T) {
	for _, reason := range []string{"turn", "timeout", "replacement", "close", "delete"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			peer := newIssue55Peer(t, false)
			llm := newMockLLM(t)
			llm.enqueue(mcpCall(t, "fixture", "effect", `{}`), reply("stopped"))
			timeout := "0"
			if reason == "timeout" {
				timeout = "2s"
			}
			a := start(t, llm, withEnv("ACPLAUSE_MCP_TIMEOUT="+timeout))
			a.initialize()
			cwd := t.TempDir()
			cfg := map[string]any{"type": "http", "name": "fixture", "url": peer.url, "headers": []any{}}
			sid := mcpSession(t, a, cwd, cfg)
			a.accepted(a.prompt(sid, "effect"))
			issue55Wait(t, peer.entered, "accepted tools/call")
			close(peer.change)
			issue55Wait(t, peer.published, "catalog publication")
			wantStatus, wantStop := "cancelled", "cancelled"
			switch reason {
			case "turn":
				a.notify("session/cancel", map[string]string{"sessionId": sid})
			case "timeout":
				wantStatus, wantStop = "failed", "end_turn"
			case "replacement":
				cfg["name"] = "replacement"
				a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{cfg}}, nil)
				wantStatus, wantStop = "failed", "end_turn"
			case "close", "delete":
				a.ok("session/"+reason, map[string]string{"sessionId": sid}, nil)
			}
			issue55Wait(t, peer.cancelled, "HTTP request cancellation")
			updates := a.untilIdle(sid)
			found := false
			for _, u := range updates {
				found = found || u.is("tool_call_update") && u.Status == wantStatus
			}
			if !found || updates[len(updates)-1].StopReason != wantStop || peer.calls.Load() != 1 {
				t.Fatalf("cancellation=%s tools/call count=%d updates=%v", reason, peer.calls.Load(), kinds(updates))
			}
		})
	}
}
