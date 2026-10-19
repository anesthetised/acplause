package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
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
				peer := newIssue55Peer(t, change == "removal")
				model := make(scriptedModel, 2)
				model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
					for _, tool := range req.Tools {
						if strings.HasPrefix(tool.Function.Name, "mcp_") {
							return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "effect", Function: llm.FunctionCall{Name: tool.Function.Name, Arguments: `{}`}}}}, nil
						}
					}
					t.Error("MCP tool missing")
					return &llm.Completion{}, nil
				}
				requests := make(chan llm.Request, 1)
				model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
					requests <- req
					return &llm.Completion{FinishReason: llm.FinishStop}, nil
				}
				c := startAgent(t, t.TempDir(), model, nil)
				permission, answer := make(chan struct{}), make(chan struct{}, 1)
				defer close(answer)
				if phase == "permission" {
					c.mu.Lock()
					c.answer = func(context.Context) (string, string) {
						close(permission)
						<-answer
						return acp.OutcomeSelected, optionAllow
					}
					c.mu.Unlock()
				}
				c.initialize()
				var session acp.NewSessionResponse
				c.ok(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), MCPServers: []acp.MCPServer{{Type: "http", Name: "fixture", URL: peer.url}}}, &session)
				sid := session.SessionID
				c.selectModel(sid, "m")
				c.ok(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.Text("effect")}}, nil)
				if phase == "permission" {
					issue55Wait(t, permission, "permission request")
				} else {
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
				updates := c.untilIdle(sid)
				wantStatus, wantCalls := string(acp.ToolCompleted), int32(1)
				if phase == "permission" {
					wantStatus, wantCalls = string(acp.ToolFailed), 0
				}
				found := false
				for _, n := range updates {
					if n.Update.Kind == "tool_call_update" && n.Update.Status == wantStatus {
						found = true
					}
				}
				if !found || peer.calls.Load() != wantCalls || lastIdle(updates).Update.StopReason != string(acp.StopEndTurn) {
					t.Errorf("want tool status=%s calls=%d; found=%v calls=%d stopReason=%s", wantStatus, wantCalls, found, peer.calls.Load(), lastIdle(updates).Update.StopReason)
				}
				select {
				case req := <-requests:
					assertToolPairs(t, req.Messages)
					last := req.Messages[len(req.Messages)-1]
					if last.ToolCallID != "effect" || (phase == "dispatched" && !strings.Contains(last.Content, "original-effect")) {
						t.Errorf("original result lost from model history: %+v", last)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("tool result never reached next model round")
				}
				if phase == "permission" {
					answer <- struct{}{}
					model.reply("late approval ignored", llm.FinishStop, nil)
					c.ok(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.Text("continue")}}, nil)
					c.untilIdle(sid)
					if peer.calls.Load() != 0 {
						t.Fatal("late approval dispatched an obsolete call")
					}
				}
			})
		}
	}
}

func TestIssue55MCPAdmittedCallRetainsSessionCancellation(t *testing.T) {
	for _, reason := range []string{"turn", "timeout", "replacement", "close", "delete", "ownership"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			peer := newIssue55Peer(t, false)
			model := make(scriptedModel, 2)
			model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				for _, tool := range req.Tools {
					if strings.HasPrefix(tool.Function.Name, "mcp_") {
						return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "effect", Function: llm.FunctionCall{Name: tool.Function.Name, Arguments: `{}`}}}}, nil
					}
				}
				t.Error("MCP tool missing")
				return &llm.Completion{}, nil
			}
			model.reply("stopped", llm.FinishStop, nil)
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			cwd := t.TempDir()
			cfg := acp.MCPServer{Type: "http", Name: "fixture", URL: peer.url}
			var session acp.NewSessionResponse
			c.ok(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd, MCPServers: []acp.MCPServer{cfg}}, &session)
			sid := session.SessionID
			c.agent.mu.Lock()
			s := c.agent.sessions[sid]
			c.agent.mu.Unlock()
			if reason == "timeout" {
				s.deps.mcpTimeout = 2 * time.Second
			}
			c.selectModel(sid, "m")
			c.ok(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.Text("effect")}}, nil)
			issue55Wait(t, peer.entered, "accepted tools/call")
			close(peer.change)
			issue55Wait(t, peer.published, "catalog publication")
			wantStatus, wantStop := string(acp.ToolCancelled), string(acp.StopCancelled)
			switch reason {
			case "turn":
				c.notify(acp.MethodSessionCancel, acp.CancelSessionNotification{SessionID: sid})
			case "timeout":
				wantStatus, wantStop = string(acp.ToolFailed), string(acp.StopEndTurn)
			case "replacement":
				cfg.Name = "replacement"
				c.ok(acp.MethodSessionResume, acp.ResumeSessionRequest{SessionID: sid, Cwd: cwd, MCPServers: []acp.MCPServer{cfg}}, nil)
				wantStatus, wantStop = string(acp.ToolFailed), string(acp.StopEndTurn)
			case "close":
				c.ok(acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: sid}, nil)
			case "delete":
				c.ok(acp.MethodSessionDelete, acp.DeleteSessionRequest{SessionID: sid}, nil)
			case "ownership":
				c.agent.drop(sid)
			}
			issue55Wait(t, peer.cancelled, "HTTP request cancellation")
			updates := c.untilIdle(sid)
			found := false
			var statuses []string
			for _, n := range updates {
				if n.Update.Kind == "tool_call_update" {
					statuses = append(statuses, n.Update.Status)
					// Ownership loss cancels sibling peer/turn contexts. The peer
					// failure may settle first; the turn must still end cancelled.
					found = found || n.Update.Status == wantStatus || reason == "ownership" && n.Update.Status == string(acp.ToolFailed)
				}
			}
			if !found || lastIdle(updates).Update.StopReason != wantStop || peer.calls.Load() != 1 {
				t.Fatalf("cancellation=%s statuses=%v stop=%s tools/call count=%d", reason, statuses, lastIdle(updates).Update.StopReason, peer.calls.Load())
			}
			s.mu.Lock()
			history := append([]llm.Message(nil), s.history...)
			s.mu.Unlock()
			assertToolPairs(t, history)
		})
	}
}
