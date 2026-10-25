package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func assertTurnTimeoutUpdates(t *testing.T, updates []update) {
	t.Helper()
	idles := 0
	for _, u := range filter(updates, "state_update") {
		if u.State == "idle" {
			idles++
		}
	}
	notices := filter(updates, "notice")
	if idle := updates[len(updates)-1]; idles != 1 || idle.StopReason != "cancelled" || idle.Error != nil || len(notices) != 1 || notices[0].Title != "Turn time limit reached" || notices[0].Severity != "warning" || !strings.Contains(notices[0].Description, "1s") {
		t.Fatalf("turn timeout lifecycle: %v", kinds(updates))
	}
}

func TestTurnTimeoutCancelsWorkAndAllowsFollowingPrompt(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"model", "permission", "form", testShellName(), "compact"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, compactionModels)
			opts := []option{withEnv("ACPLAUSE_TURN_TIMEOUT=1s")}
			if phase == "permission" {
				opts = append(opts, askingUser)
			}
			var a *agent
			var sid, cwd string
			base := 0
			if phase == "compact" {
				a, sid, cwd = seedCommandHistory(t, llm, opts...)
				base = 2
			} else if phase == "form" {
				a, sid, cwd = startedForms(t, llm, opts...)
			} else {
				a, sid, cwd = started(t, llm, opts...)
			}
			aborted := make(chan struct{})
			switch phase {
			case "model", "compact":
				llm.enqueue(waitFor(nil, aborted))
			case "permission":
				llm.enqueue(callTool("write_file", `{"path":"changed","content":"bad"}`))
			case "form":
				llm.enqueue(callTool("ask_user", formArgs))
			case testShellName():
				llm.enqueue(func(s *stream) {
					s.toolCall(0, "waiting", testShellName(), testShellArgs(`echo ready; sleep 30`, "[Console]::Write(\"ready`n\"); Start-Sleep -Seconds 30"))
					s.toolCall(1, "never", "write_file", `{"path":"changed","content":"bad"}`)
					s.finish("tool_calls")
				})
			}
			prompt := "wait"
			if phase == "compact" {
				prompt = "/compact"
			}
			a.accepted(a.prompt(sid, prompt))
			var waiting *message
			if phase == "permission" {
				waiting = a.permissionRequest()
			} else if phase == "form" {
				waiting = a.elicitationRequest()
			}
			updates := a.untilIdle(sid)
			assertTurnTimeoutUpdates(t, updates)
			if phase == "model" || phase == "compact" {
				waitClosed(t, aborted, "deadline did not abort model HTTP stream")
			}
			if phase == "form" {
				cancel := a.take(func(m *message) bool { return m.Method == "$/cancel_request" })
				var params struct{ RequestID json.RawMessage }
				if err := json.Unmarshal(cancel.Params, &params); err != nil || string(params.RequestID) != string(waiting.ID) {
					t.Fatalf("form cancellation: %s, %v", cancel.Params, err)
				}
				a.answer(waiting, formAnswer("accept"))
			} else if phase == "permission" {
				a.answer(waiting, selected(optionOfKind(t, waiting, "allow_once")))
			}
			if phase == "compact" {
				compactions := filter(updates, "compaction_update")
				if len(compactions) != 2 || compactions[1].Status != "cancelled" {
					t.Fatalf("deadline committed a summary: %v", kinds(updates))
				}
			}
			if llm.requestCount() != base+1 {
				t.Fatal("deadline started another request")
			}
			llm.enqueue(reply("Continued."))
			a.accepted(a.prompt(sid, "continue"))
			next := a.untilIdle(sid)
			if idle := next[len(next)-1]; idle.StopReason != "end_turn" || len(filter(next, "notice")) != 0 {
				t.Fatalf("deadline allowance did not reset: %v", kinds(next))
			}
			if phase == "permission" || phase == "form" || phase == testShellName() {
				count := 0
				for _, m := range llm.request(base + 1).Messages {
					if m["role"] == "tool" {
						count++
						if !strings.Contains(m["content"].(string), "Cancel") {
							t.Fatalf("timed-out tool result: %+v", m)
						}
					}
				}
				want := 1
				if phase == testShellName() {
					want = 2
				}
				if count != want {
					t.Fatalf("missing sibling results: got %d, want %d", count, want)
				}
			}
			if _, err := os.Stat(filepath.Join(cwd, "changed")); !os.IsNotExist(err) {
				t.Fatalf("late approval or sibling wrote: %v", err)
			}
			a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			if replay := a.untilIdle(sid); len(filter(replay, "notice")) != 0 {
				t.Fatal("timeout notice entered replay")
			}
		})
	}
}

func TestTurnTimeoutCancelsMCPWithoutReplay(t *testing.T) {
	t.Parallel()
	entered, aborted := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "wait", "description": "fixture:wait", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			if calls.Add(1) != 1 {
				t.Error("ambiguous MCP call was replayed")
				return
			}
			close(entered)
			<-r.Context().Done()
			close(aborted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "timeout", "wait", `{}`))
	a := start(t, llm, withEnv("ACPLAUSE_TURN_TIMEOUT=1s", "ACPLAUSE_MCP_TIMEOUT=0"))
	a.initialize()
	cwd := t.TempDir()
	sid := mcpSession(t, a, cwd, map[string]any{"type": "http", "name": "timeout", "url": srv.URL})
	a.accepted(a.prompt(sid, "wait remotely"))
	waitClosed(t, entered, "MCP call was never dispatched")
	assertTurnTimeoutUpdates(t, a.untilIdle(sid))
	waitClosed(t, aborted, "deadline did not cancel MCP HTTP request")
	llm.enqueue(reply("Continued."))
	a.accepted(a.prompt(sid, "continue"))
	a.untilIdle(sid)
	if calls.Load() != 1 || llm.requestCount() != 2 {
		t.Fatal("MCP timeout retried work or blocked next turn")
	}
}

func TestTurnTimeoutDisabledAndUserLifecycleCancellation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ limit, action string }{
		{"0", "cancel"}, {"1s", "cancel"}, {"1s", "close"}, {"1s", "delete"},
	} {
		t.Run(tc.limit+"/"+tc.action, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			aborted := make(chan struct{})
			llm.enqueue(waitFor(nil, aborted))
			a, sid, _ := started(t, llm, withEnv("ACPLAUSE_TURN_TIMEOUT="+tc.limit))
			a.accepted(a.prompt(sid, "wait"))
			a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
			if tc.action == "cancel" {
				a.notify("session/cancel", map[string]any{"sessionId": sid})
			} else {
				a.ok("session/"+tc.action, map[string]any{"sessionId": sid}, nil)
			}
			updates := a.untilIdle(sid)
			if idle := updates[len(updates)-1]; idle.StopReason != "cancelled" || idle.Error != nil || len(filter(updates, "notice")) != 0 {
				t.Fatalf("user lifecycle cancellation became a timeout: %v", kinds(updates))
			}
			waitClosed(t, aborted, "lifecycle cancellation did not abort HTTP")
			if llm.requestCount() != 1 {
				t.Fatal("lifecycle cancellation started another request")
			}
		})
	}
}
