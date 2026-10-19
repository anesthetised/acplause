package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const formArgs = `{"message":"Choose <&>","questions":[{"id":"choice","title":"Choice","question":"Which?","options":["a","b"]},{"id":"detail","title":"Detail","question":"Why?"},{"id":"last","title":"Last","question":"Anything else?"}]}`

func (a *agent) initializeForms() {
	a.t.Helper()
	a.ok("initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "forms", "version": "1"}, "capabilities": map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}}, nil)
}

// elicitationRequest leaves forms for the test to answer; permission requests
// retain the harness's automatic approval behavior.
func (a *agent) elicitationRequest() *message {
	a.t.Helper()
	return a.take(func(m *message) bool { return m.Method == "elicitation/create" })
}

func startedForms(t *testing.T, llm *mockLLM, opts ...option) (*agent, string, string) {
	t.Helper()
	a := start(t, llm, opts...)
	a.initializeForms()
	cwd := t.TempDir()
	sid := a.newSession(cwd).SessionID
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
	return a, sid, cwd
}

func formAnswer(action string) map[string]any {
	return map[string]any{"action": action, "content": map[string]any{"choice": "a", "detail": "<&>", "last": ""}}
}

func TestElicitationAnswers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		result  any
		success bool
	}{
		{"accept", formAnswer("accept"), true}, {"decline", formAnswer("decline"), false},
		{"cancel", formAnswer("cancel"), false}, {"unknown", formAnswer("_future"), false},
		{"missing required", map[string]any{"action": "accept", "content": map[string]any{"choice": "a"}}, false},
		{"enum mismatch", map[string]any{"action": "accept", "content": map[string]any{"choice": "c", "detail": "ok", "last": ""}}, false},
		{"wrong type", map[string]any{"action": "accept", "content": map[string]any{"choice": "a", "detail": false, "last": ""}}, false},
		{"extra key", map[string]any{"action": "accept", "content": map[string]any{"choice": "a", "detail": "ok", "last": "", "extra": "x"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("ask_user", formArgs), reply("Done."))
			a, sid, _ := startedForms(t, llm)
			a.accepted(a.prompt(sid, "ask"))
			q := a.elicitationRequest()
			var req struct {
				Mode, SessionID, ToolCallID, Message string
				RequestedSchema                      struct {
					Type       string
					Properties map[string]struct {
						Type, Title, Description string
						Enum                     []string
					}
					Required []string
				}
			}
			if err := json.Unmarshal(q.Params, &req); err != nil {
				t.Fatal(err)
			}
			if req.Mode != "form" || req.SessionID != sid || req.ToolCallID == "" || req.Message != "Choose <&>" || req.RequestedSchema.Type != "object" || len(req.RequestedSchema.Properties) != 3 || !slices.Equal(req.RequestedSchema.Required, []string{"choice", "detail", "last"}) || !slices.Equal(req.RequestedSchema.Properties["choice"].Enum, []string{"a", "b"}) {
				t.Fatalf("form: %s", q.Params)
			}
			a.answer(q, tt.result)
			updates := a.untilIdle(sid)
			var states []string
			for _, u := range filter(updates, "state_update") {
				states = append(states, u.State)
			}
			if !slices.Equal(states, []string{"running", "requires_action", "running", "idle"}) {
				t.Fatalf("states: %v", states)
			}
			calls := filter(updates, "tool_call_update")
			want := "failed"
			if tt.success {
				want = "completed"
			}
			if len(calls) != 2 || calls[0].Status != "pending" || calls[1].Status != want || calls[0].ToolCallID != req.ToolCallID {
				t.Fatalf("tool updates: %+v", calls)
			}
			r := llm.request(1)
			last := r.Messages[len(r.Messages)-1]
			if last["role"] != "tool" || (tt.success != strings.Contains(last["content"].(string), `<&>`)) {
				t.Fatalf("model result: %+v", last)
			}
			a.expectQuiet(func(m *message) bool {
				return m.Method == "session/request_permission" || m.Method == "elicitation/create"
			})
		})
	}
}

func TestElicitationUnavailableOrInvalid(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, args string
		capable    bool
	}{
		{"no capability", formArgs, false}, {"invalid fourth", strings.Replace(formArgs, `"questions":[`, `"questions":[{"id":"four","title":"4","question":"4"},`, 1), true},
		{"duplicate IDs", strings.Replace(formArgs, `"id":"detail"`, `"id":"choice"`, 1), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("ask_user", tt.args), reply("Done."))
			var a *agent
			var sid string
			if tt.capable {
				a, sid, _ = startedForms(t, llm)
			} else {
				a, sid, _ = started(t, llm)
			}
			a.accepted(a.prompt(sid, "ask"))
			calls := filter(a.untilIdle(sid), "tool_call_update")
			if len(calls) != 1 || calls[0].Status != "failed" {
				t.Fatalf("calls: %+v", calls)
			}
			req := llm.request(0)
			found := false
			for _, tool := range req.Tools {
				if tool["function"].(map[string]any)["name"] == "ask_user" {
					found = true
				}
			}
			if found != tt.capable {
				t.Fatalf("ask_user advertised=%v", found)
			}
			a.expectNoElicitation()
		})
	}
}

func TestElicitationCancellationAndLateAnswer(t *testing.T) {
	t.Parallel()
	for _, stop := range []string{"cancel", "close", "delete"} {
		t.Run(stop, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("ask_user", formArgs))
			a, sid, _ := startedForms(t, llm)
			a.accepted(a.prompt(sid, "ask"))
			q := a.elicitationRequest()
			if stop == "cancel" {
				a.notify("session/cancel", map[string]any{"sessionId": sid})
			} else {
				a.ok("session/"+stop, map[string]any{"sessionId": sid}, nil)
			}
			cancel := a.take(func(m *message) bool { return m.Method == "$/cancel_request" })
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(cancel.Params, &params)
			if string(params.RequestID) != string(q.ID) {
				t.Fatalf("cancellation %s for %s", cancel.Params, q.ID)
			}
			updates := a.untilIdle(sid)
			calls := filter(updates, "tool_call_update")
			if len(calls) != 2 || calls[1].Status != "cancelled" || updates[len(updates)-1].StopReason != "cancelled" {
				t.Fatalf("cancel updates: %v", updates)
			}
			for _, u := range filter(updates, "state_update") {
				if u.State == "running" && u.seq > q.seq {
					t.Fatal("cancelled work resumed")
				}
			}
			a.answer(q, formAnswer("accept"))
			a.initializeForms()
			a.expectQuiet(func(m *message) bool {
				return m.Method == "elicitation/create" || (m.Method == "session/update" && decodeUpdate(t, m).State == "running")
			})
			if llm.requestCount() != 1 {
				t.Fatalf("late answer started another model round")
			}
		})
	}
}

func TestElicitationResumeDoesNotReissue(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("ask_user", formArgs), reply("Done."))
	a, sid, cwd := startedForms(t, llm)
	a.accepted(a.prompt(sid, "ask"))
	q := a.elicitationRequest()
	a.untilState(sid, "requires_action")
	resp := a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := a.updateWhere(sid, func(u update) bool { return u.is("tool_call_update") })
	snapshot := a.updateWhere(sid, func(u update) bool { return u.is("state_update") })
	if replay.Status != "pending" || replay.seq > resp.seq || snapshot.State != "requires_action" || snapshot.seq > resp.seq {
		t.Fatalf("replay=%+v snapshot=%+v", replay, snapshot)
	}
	a.expectNoElicitation()
	a.answer(q, formAnswer("accept"))
	a.untilIdle(sid)
}

func (a *agent) untilState(sid, state string) {
	a.t.Helper()
	for {
		if u := a.nextUpdate(sid); u.is("state_update") && u.State == state {
			return
		}
	}
}

func TestElicitationRestart(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("ask_user", formArgs))
	a, sid, cwd := startedForms(t, llm, withDataDir(data))
	a.accepted(a.prompt(sid, "ask"))
	a.elicitationRequest()
	a.untilState(sid, "requires_action")
	a.stop()
	b := start(t, llm, withDataDir(data))
	b.initializeForms()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	calls := filter(b.untilIdle(sid), "tool_call_update")
	if len(calls) != 1 || calls[0].Name != "ask_user" || calls[0].Status != "cancelled" {
		t.Fatalf("replay: %+v", calls)
	}
	b.expectNoElicitation()
	llm.enqueue(reply("Continue."))
	b.accepted(b.prompt(sid, "continue"))
	b.untilIdle(sid)
	req := llm.request(1)
	var toolResult bool
	for _, m := range req.Messages {
		if m["role"] == "tool" && strings.Contains(m["content"].(string), "Cancelled") {
			toolResult = true
		}
	}
	if !toolResult {
		t.Fatalf("missing matching cancelled tool result: %+v", req.Messages)
	}
}

func (a *agent) expectNoElicitation() {
	a.t.Helper()
	for _, m := range a.pending {
		if m.Method == "elicitation/create" {
			a.t.Fatalf("unexpected elicitation: %s", m.Params)
		}
	}
	a.expectQuiet(func(m *message) bool { return m.Method == "elicitation/create" })
}

func TestElicitationAnswerDoesNotGrantPermission(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("ask_user", formArgs), callTool("write_file", `{"path":"answer.txt","content":"written"}`), reply("Done."))
	a, sid, cwd := startedForms(t, llm, askingUser)
	a.accepted(a.prompt(sid, "ask then write"))
	a.answer(a.elicitationRequest(), formAnswer("accept"))
	permission := a.permissionRequest()
	if _, err := os.Stat(filepath.Join(cwd, "answer.txt")); !os.IsNotExist(err) {
		t.Fatalf("write ran without permission: %v", err)
	}
	a.answer(permission, selected(optionOfKind(t, permission, "reject_once")))
	calls := filter(a.untilIdle(sid), "tool_call_update")
	if calls[len(calls)-1].Status != "failed" {
		t.Fatalf("write status: %+v", calls)
	}
	if _, err := os.Stat(filepath.Join(cwd, "answer.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected write ran: %v", err)
	}
}

func TestElicitationMalformedCapabilities(t *testing.T) {
	t.Parallel()
	for _, caps := range []string{
		`true`,
		`[]`,
		`{"elicitation":false}`,
		`{"elicitation":[]}`,
		`{"elicitation":{"form":true}}`,
		`{"elicitation":{"form":[]}}`,
	} {
		t.Run(caps, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(reply("Ready."))
			a := start(t, llm)
			a.ok("initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "e2e", "version": "1"}, "capabilities": json.RawMessage(caps)}, nil)
			sid := a.newSession(t.TempDir()).SessionID
			a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
			a.accepted(a.prompt(sid, "hello"))
			a.untilIdle(sid)
			for _, tool := range llm.request(0).Tools {
				if tool["function"].(map[string]any)["name"] == "ask_user" {
					t.Fatal("malformed capability advertised form support")
				}
			}
			a.expectNoElicitation()
		})
	}
}
