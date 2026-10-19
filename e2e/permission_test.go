package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// permission is a decoded session/request_permission.
type permission struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Subject   struct {
		Type       string `json:"type"`
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		ToolCallID string `json:"toolCallId"`
		ToolCall   *struct {
			ToolCallID string          `json:"toolCallId"`
			Status     string          `json:"status"`
			Content    json.RawMessage `json:"content"`
		} `json:"toolCall"`
	} `json:"subject"`
	Options []struct{ OptionID, Name, Kind string } `json:"options"`
}

func decodePermission(t *testing.T, m *message) permission {
	t.Helper()
	var p permission
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func selected(option string) map[string]any {
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option}}
}

func TestPermissionAllowsFileWrite(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("write_file", `{"path":"a.txt","content":"hi\n"}`), reply("Written."))
	a, sid, cwd := started(t, llm, askingUser)
	a.accepted(a.prompt(sid, "write"))

	req := a.permissionRequest()
	p := decodePermission(t, req)
	optionKinds := []string{p.Options[0].Kind, p.Options[1].Kind}
	if p.SessionID != sid || p.Title != "Write a.txt?" || len(p.Options) != 2 ||
		!slices.Equal(optionKinds, []string{"allow_once", "reject_once"}) {
		t.Fatalf("permission request: %+v", p)
	}
	if p.Subject.Type != "tool_call" || p.Subject.ToolCall == nil || p.Subject.ToolCall.Status != "pending" ||
		!strings.Contains(string(p.Subject.ToolCall.Content), `"operation":"add"`) {
		t.Fatalf("subject: %s", req.Params)
	}
	if _, err := os.Stat(filepath.Join(cwd, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("file written before permission: %v", err)
	}
	a.answer(req, selected(optionOfKind(t, req, "allow_once")))

	updates := a.untilIdle(sid)
	want := []string{
		"user_message", "state_update running", "usage_update",
		"tool_call_update pending", "state_update requires_action", "state_update running",
		"tool_call_update in_progress", "tool_call_update completed",
		"agent_message_chunk", "usage_update", "state_update idle end_turn",
	}
	if got := kinds(updates); !slices.Equal(got, want) {
		t.Fatalf("updates:\n got %v\nwant %v", got, want)
	}
	pending := filter(updates, "tool_call_update")[0]
	if pending.ToolCallID != p.Subject.ToolCall.ToolCallID || pending.Kind != "edit" || pending.Name != "write_file" {
		t.Fatalf("pending call: %+v", pending)
	}
	if data, err := os.ReadFile(filepath.Join(cwd, "a.txt")); err != nil || string(data) != "hi\n" {
		t.Fatalf("file after approval: %q, %v", data, err)
	}
}

func TestPermissionForCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer func(t *testing.T, req *message) map[string]any
		runs   bool
		result string // in the tool result the model gets
	}{
		{"allowed", func(t *testing.T, req *message) map[string]any { return selected(optionOfKind(t, req, "allow_once")) }, true, "ran"},
		{"rejected", func(t *testing.T, req *message) map[string]any { return selected(optionOfKind(t, req, "reject_once")) }, false, "the user rejected this tool call"},
		{"cancelled outcome", func(*testing.T, *message) map[string]any {
			return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		}, false, "permission was not granted"},
		{"unknown outcome", func(*testing.T, *message) map[string]any {
			return map[string]any{"outcome": map[string]any{"outcome": "_later"}}
		}, false, "permission was not granted"},
		{"unknown option", func(*testing.T, *message) map[string]any { return selected("_maybe") }, false, "permission was not granted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("bash", `{"command":"echo ran; touch ran"}`), reply("ok"))
			a, sid, cwd := started(t, llm, askingUser)
			a.accepted(a.prompt(sid, "run"))

			req := a.permissionRequest()
			p := decodePermission(t, req)
			if p.Subject.Type != "command" || p.Subject.Command != "echo ran; touch ran" || p.Subject.Cwd != cwd || p.Subject.ToolCallID == "" {
				t.Fatalf("subject: %s", req.Params)
			}
			a.answer(req, tt.answer(t, req))

			updates := a.untilIdle(sid)
			calls := filter(updates, "tool_call_update")
			last, idle := calls[len(calls)-1], updates[len(updates)-1]
			if calls[0].Kind != "execute" || calls[0].ToolCallID != p.Subject.ToolCallID {
				t.Fatalf("pending call: %+v", calls[0])
			}
			if wantStatus := map[bool]string{true: "completed", false: "failed"}[tt.runs]; last.Status != wantStatus || idle.StopReason != "end_turn" {
				t.Fatalf("updates: %v", kinds(updates))
			}
			if _, err := os.Stat(filepath.Join(cwd, "ran")); os.IsNotExist(err) == tt.runs {
				t.Fatalf("command ran: %v, want %v", !os.IsNotExist(err), tt.runs)
			}
			msgs := llm.request(1).Messages
			if result := msgs[len(msgs)-1]; result["role"] != "tool" || !strings.Contains(result["content"].(string), tt.result) {
				t.Fatalf("tool result: %v", result)
			}
		})
	}
}

func TestPermissionErrorResponseIsNotApproval(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("bash", `{"command":"touch ran"}`), reply("ok"))
	a, sid, cwd := started(t, llm, askingUser)
	a.accepted(a.prompt(sid, "run"))
	req := a.permissionRequest()
	a.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32603, "message": "no UI"}})
	calls := filter(a.untilIdle(sid), "tool_call_update")
	if last := calls[len(calls)-1]; last.Status != "failed" || !strings.Contains(last.text(), "no UI") {
		t.Fatalf("call: %+v", last)
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran")); !os.IsNotExist(err) {
		t.Fatal("command ran without permission")
	}
}

func TestCancelWhileAwaitingPermission(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("bash", `{"command":"touch ran"}`), reply("next"))
	a, sid, cwd := started(t, llm, askingUser)
	a.accepted(a.prompt(sid, "run"))
	req := a.permissionRequest()
	a.updateWhere(sid, func(u update) bool { return u.is("state_update") && u.State == "requires_action" })

	// Resuming while blocked reports the blocked state and the pending call.
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replayed := a.updateWhere(sid, func(u update) bool { return u.is("tool_call_update") && u.Status == "pending" })
	snapshot := a.updateWhere(sid, func(u update) bool { return u.is("state_update") && u.seq > replayed.seq })
	if snapshot.State != "requires_action" {
		t.Fatalf("resume snapshot: %+v", snapshot)
	}

	// The client never answers: cancellation must not wait for it, and the
	// turn ends without reporting running again.
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	after := slices.DeleteFunc(a.untilIdle(sid), func(u update) bool { return u.seq < snapshot.seq })
	if got := kinds(after); !slices.Equal(got, []string{"tool_call_update cancelled", "state_update idle cancelled"}) {
		t.Fatalf("updates after cancel: %v", got)
	}

	// A late answer does not resume the cancelled call.
	a.answer(req, selected(optionOfKind(t, req, "allow_once")))
	a.expectQuiet(func(m *message) bool { return m.Method == "session/update" })
	if _, err := os.Stat(filepath.Join(cwd, "ran")); !os.IsNotExist(err) {
		t.Fatal("cancelled command ran")
	}
	a.accepted(a.prompt(sid, "again"))
	if idle := a.untilIdle(sid); idle[len(idle)-1].StopReason != "end_turn" {
		t.Fatalf("next turn: %v", kinds(idle))
	}
}

func TestFailingCallIsNotAskedAbout(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("edit", `{"path":"a.txt","old_text":"nope","new_text":"x"}`), reply("ok"))
	a, sid, cwd := started(t, llm, askingUser)
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.accepted(a.prompt(sid, "edit"))
	updates := a.untilIdle(sid)
	calls := filter(updates, "tool_call_update")
	if len(calls) != 1 || calls[0].Status != "failed" || !strings.Contains(calls[0].text(), "not found") {
		t.Fatalf("calls: %v", calls)
	}
	if slices.ContainsFunc(updates, func(u update) bool { return u.State == "requires_action" }) {
		t.Fatalf("asked about a failing call: %v", kinds(updates))
	}
	a.expectQuiet(func(m *message) bool { return m.Method == "session/request_permission" })
}
