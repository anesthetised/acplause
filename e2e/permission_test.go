package e2e

import (
	"encoding/json"
	"fmt"
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

// cancellation takes the next cancellation and checks which request it targets.
func (a *agent) cancellation(req *message) *message {
	a.t.Helper()
	m := a.take(func(m *message) bool { return m.Method == "$/cancel_request" })
	var p struct{ RequestID json.RawMessage }
	if err := json.Unmarshal(m.Params, &p); err != nil || string(p.RequestID) != string(req.ID) {
		a.t.Fatalf("cancellation %s for request %s: %v", m.Params, req.ID, err)
	}
	return m
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
		"user_message", "session_info_update", "state_update running", "usage_update", "session_info_update",
		"tool_call_update pending", "state_update requires_action", "state_update running",
		"tool_call_update in_progress", "tool_call_update completed", "session_info_update",
		"agent_message_chunk", "usage_update", "session_info_update", "state_update idle end_turn",
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
			llm.enqueue(callTool(testShellName(), testShellArgs(`echo ran; touch ran`, "[Console]::Write(\"ran`n\"); [IO.File]::WriteAllText('ran', '')")), reply("ok"))
			a, sid, cwd := started(t, llm, askingUser)
			a.accepted(a.prompt(sid, "run"))

			req := a.permissionRequest()
			p := decodePermission(t, req)
			if p.Subject.Type != "command" || p.Subject.Command != testShellCommand(`echo ran; touch ran`, "[Console]::Write(\"ran`n\"); [IO.File]::WriteAllText('ran', '')") || p.Subject.Cwd != cwd || p.Subject.ToolCallID == "" {
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
	llm.enqueue(callTool(testShellName(), testShellArgs(`touch ran`, `[IO.File]::WriteAllText('ran', '')`)), reply("ok"))
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
	run := callTool(testShellName(), testShellArgs(`touch ran`, `[IO.File]::WriteAllText('ran', '')`))
	llm.enqueue(run, run, reply("next"))
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
	cancel := a.cancellation(req)
	after := slices.DeleteFunc(a.untilIdle(sid), func(u update) bool { return u.seq < snapshot.seq })
	if got := kinds(after); !slices.Equal(got, []string{"tool_call_update cancelled", "session_info_update", "state_update idle cancelled"}) {
		t.Fatalf("updates after cancel: %v", got)
	}

	if cancel.seq >= after[0].seq {
		t.Fatal("cancelled tool update preceded cancellation of its permission")
	}

	// A late always-allow neither executes the command nor authorizes a retry.
	a.answer(req, selected(optionOfKind(t, req, "allow_always")))
	a.accepted(a.prompt(sid, "again"))
	retry := a.permissionRequest()
	a.answer(retry, selected(optionOfKind(t, retry, "reject_once")))
	if idle := a.untilIdle(sid); idle[len(idle)-1].StopReason != "end_turn" {
		t.Fatalf("next turn: %v", kinds(idle))
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran")); !os.IsNotExist(err) {
		t.Fatalf("cancelled command ran: %v", err)
	}
	var results []string
	for _, m := range llm.request(1).Messages {
		if m["role"] == "tool" && m["tool_call_id"] == "call_"+testShellName() {
			results = append(results, m["content"].(string))
		}
	}
	if !slices.Equal(results, []string{"Cancelled by the user."}) {
		t.Fatalf("cancelled tool history: %v", results)
	}
}

func TestCancelAndAllowInOneWrite(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("write_file", `{"path":"ran","content":"allowed"}`))
	a, sid, cwd := started(t, llm, askingUser)
	a.accepted(a.prompt(sid, "write"))
	req := a.permissionRequest()
	a.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}
{"jsonrpc":"2.0","id":%s,"result":{"outcome":{"outcome":"selected","optionId":%q}}}`,
		sid, req.ID, optionOfKind(t, req, "allow_once")))
	updates := a.untilIdle(sid)
	cancelled := filter(updates, "tool_call_update")
	if idle := updates[len(updates)-1]; idle.StopReason != "cancelled" || len(cancelled) == 0 || cancelled[len(cancelled)-1].Status != "cancelled" {
		t.Fatalf("turn after cancellation and allow: %v", kinds(updates))
	}
	if a.cancellation(req).seq >= cancelled[len(cancelled)-1].seq {
		t.Fatal("tool cancellation preceded permission cancellation")
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran")); !os.IsNotExist(err) {
		t.Fatalf("cancelled permission allowed a write: %v", err)
	}
}

func TestCloseCancelsOnlyItsPendingPermission(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	write := callTool("write_file", `{"path":"ran","content":"allowed"}`)
	llm.enqueue(write, write, reply("done"))
	a, sid, cwd := started(t, llm, askingUser)
	otherDir := t.TempDir()
	other := a.newSession(otherDir).SessionID
	a.ok("session/set_config_option", map[string]any{"sessionId": other, "configId": "model", "type": "id", "value": "test/model"}, nil)
	a.accepted(a.prompt(sid, "write"))
	req := a.permissionRequest()
	a.accepted(a.prompt(other, "write"))
	stillPending := a.permissionRequest()

	closed := a.ok("session/close", map[string]any{"sessionId": sid}, nil)
	cancel := a.cancellation(req)
	updates := a.untilIdle(sid)
	idle := updates[len(updates)-1]
	calls := filter(updates, "tool_call_update")
	if cancel.seq >= closed.seq || idle.seq >= closed.seq || idle.StopReason != "cancelled" ||
		len(calls) != 2 || calls[1].Status != "cancelled" {
		t.Fatalf("cancel line %d, updates %v, idle line %d, close line %d", cancel.seq, kinds(updates), idle.seq, closed.seq)
	}
	a.answer(req, selected(optionOfKind(t, req, "allow_once")))
	a.answer(stillPending, selected(optionOfKind(t, stillPending, "allow_once")))
	if updates := a.untilIdle(other); updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("other session: %v", kinds(updates))
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran")); !os.IsNotExist(err) {
		t.Fatalf("closed session ran its tool: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(otherDir, "ran")); err != nil || string(got) != "allowed" {
		t.Fatalf("other session's file: %q, %v", got, err)
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

func TestCancelledCloseRepliesAfterPermissionCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("write_file", `{"path":"ran","content":"bad"}`))
	a, sid, _ := started(t, llm, askingUser, withDataDir(dir))
	a.accepted(a.prompt(sid, "write"))
	req := a.permissionRequest()
	unlock := holdStoreWriter(t, dir)

	// The first close must finish saving the turn and releasing its lease.
	// The duplicate close has to wait for that predecessor, even if cancelled.
	first := a.request("session/close", map[string]any{"sessionId": sid})
	second := a.request("session/close", map[string]any{"sessionId": sid})
	for _, id := range []string{first, second} {
		a.notify("$/cancel_request", map[string]any{"requestId": json.RawMessage(id)})
	}
	// An absent session closes immediately, fencing both cancellations on
	// the reader while this session still owns its cleanup.
	a.ok("session/close", map[string]any{"sessionId": "absent"}, nil)
	cancel := a.cancellation(req)
	unlock()
	updates := a.untilIdle(sid)
	idle := updates[len(updates)-1]
	one, two := a.response(first), a.response(second)
	if idle.StopReason != "cancelled" || cancel.seq >= idle.seq || idle.seq >= one.seq || one.seq >= two.seq ||
		one.Error == nil || one.Error.Code != -32800 || two.Error == nil || two.Error.Code != -32800 {
		t.Fatalf("cancel line %d, idle %+v, close responses %+v and %+v", cancel.seq, idle, one, two)
	}
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
}
