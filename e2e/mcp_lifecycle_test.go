package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPResumeRevokesPendingPermission(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "alpha", "schedule_task", `{}`), reply("reconfigured"))
	a := start(t, llm, askingUser)
	a.initialize()
	cwd := t.TempDir()
	log := filepath.Join(cwd, "calls")
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", log, "old"))
	a.accepted(a.prompt(sid, "call"))
	permission := a.permissionRequest()
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{mcpConfig("alpha", log, "new")}}, nil)
	updates := a.untilIdle(sid)
	if updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("wrong cancellation domain: %v", kinds(updates))
	}
	failed := false
	for _, u := range updates {
		if u.Status == "failed" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("revoked call was not failed")
	}
	a.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
	if data, _ := os.ReadFile(log); len(data) > 0 {
		t.Fatalf("old approval dispatched to a server: %s", data)
	}
	llm.enqueue(mcpCall(t, "alpha", "read_file", `{}`), reply("done"))
	a.accepted(a.prompt(sid, "new call"))
	p := a.permissionRequest()
	a.answer(p, selected(optionOfKind(t, p, "allow_once")))
	a.untilIdle(sid)
	msgs := llm.request(3).Messages
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), `"label":"new"`) {
		t.Fatal("did not use new config")
	}
}

func TestMCPCancelAndLateResponse(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "alpha", "wait", `{}`))
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	log := filepath.Join(cwd, "calls")
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", log, "first"))
	a.accepted(a.prompt(sid, "wait"))
	a.toolStarted(sid)
	// Wait until the remote has received the call before cancellation.
	waitForFile(t, log)
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	updates := a.untilIdle(sid)
	if updates[len(updates)-1].StopReason != "cancelled" {
		t.Fatal(kinds(updates))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(log)
		if strings.Contains(string(data), `"name":"notifications/cancelled"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("MCP cancellation never reached server: %s", data)
		}
		time.Sleep(time.Millisecond)
	}
	llm.enqueue(mcpCall(t, "alpha", "delegate_task", `{}`), reply("done"))
	a.accepted(a.prompt(sid, "again"))
	a.untilIdle(sid)
	for _, m := range llm.request(2).Messages {
		text, _ := m["content"].(string)
		if strings.Contains(text, "LATE_RESULT_MUST_NOT_APPEAR") {
			t.Fatal("late response entered next turn")
		}
	}
}

func TestMCPResumeThenPromptCancellation(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	sid := mcpSession(t, a, cwd)
	gate := filepath.Join(cwd, "gate")
	cfg := mcpConfig("alpha", filepath.Join(cwd, "calls"), "new")
	cfg["env"] = append(cfg["env"].([]any), map[string]string{"name": "FIXTURE_GATE", "value": gate})
	resume := a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{cfg}})
	waitForFile(t, gate+".entered")
	prompt := a.prompt(sid, "must wait for setup")
	a.expectQuiet(func(m *message) bool { return m.Method == "" && string(m.ID) == prompt })
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	resp := a.response(prompt)
	if resp.Error == nil || resp.Error.Code != -32800 {
		t.Fatalf("prompt crossed insertion during setup: result=%s err=%+v", resp.Result, resp.Error)
	}
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	if resp := a.response(resume); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if llm.requestCount() != 0 {
		t.Fatal("cancelled prompt reached model")
	}
}

func TestMCPResumeRollbackAndOrdering(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	log := filepath.Join(cwd, "calls")
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", log, "old"))
	bad := mcpConfig("bad", log, "bad")
	bad["command"] = filepath.Join(cwd, "missing-executable")
	a.fails("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{t.TempDir()}, "mcpServers": []any{mcpConfig("alpha", log, "candidate"), bad}}, -32602)
	llm.enqueue(mcpCall(t, "alpha", "read_file", `{}`), reply("done"))
	a.accepted(a.prompt(sid, "old config survived"))
	a.untilIdle(sid)
	msgs := llm.request(1).Messages
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), `"label":"old"`) {
		t.Fatal("partial setup replaced old connection")
	}
	gate := filepath.Join(cwd, "gate")
	cfg := mcpConfig("alpha", log, "first")
	cfg["env"] = append(cfg["env"].([]any), map[string]string{"name": "FIXTURE_GATE", "value": gate})
	first := a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{cfg}})
	waitForFile(t, gate+".entered")
	second := a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{mcpConfig("alpha", log, "last")}})
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := a.response(first); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := a.response(second); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Consume the two snapshots emitted by successful resumes.
	for range 2 {
		a.updateWhere(sid, func(u update) bool { return u.is("state_update") && u.State == "idle" })
	}
	llm.enqueue(mcpCall(t, "alpha", "read_file", `{}`), reply("done"))
	a.accepted(a.prompt(sid, "latest config"))
	a.untilIdle(sid)
	msgs = llm.request(3).Messages
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), `"label":"last"`) {
		t.Fatal("resume order was inverted")
	}
}

func TestMCPTimeoutIsIndependent(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "alpha", "wait", `{}`), reply("timed out"))
	a := start(t, llm, withEnv("ACPLAUSE_MCP_TIMEOUT=50ms", "ACPLAUSE_BASH_TIMEOUT=1h"))
	a.initialize()
	cwd := t.TempDir()
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", filepath.Join(cwd, "calls"), "first"))
	a.accepted(a.prompt(sid, "timeout"))
	updates := a.untilIdle(sid)
	if updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatal(kinds(updates))
	}
	found := false
	for _, u := range updates {
		if u.Status == "failed" && strings.Contains(u.text(), "deadline") {
			found = true
		}
	}
	if !found {
		t.Fatal("MCP tool did not use independent deadline")
	}
}

func TestMCPRequestCancellationDoesNotCancelNextPrompt(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("second ran"))
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	sid := mcpSession(t, a, cwd)
	gate := filepath.Join(cwd, "gate")
	cfg := mcpConfig("alpha", filepath.Join(cwd, "calls"), "new")
	cfg["env"] = append(cfg["env"].([]any), map[string]string{"name": "FIXTURE_GATE", "value": gate})
	resume := a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{cfg}})
	waitForFile(t, gate+".entered")
	first := a.prompt(sid, "first")
	second := a.prompt(sid, "second")
	a.notify("$/cancel_request", map[string]any{"requestId": json.RawMessage(first)})
	if r := a.response(first); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("first: %+v", r)
	}
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := a.response(resume); r.Error != nil {
		t.Fatal(r.Error)
	}
	a.updateWhere(sid, func(u update) bool { return u.is("state_update") && u.State == "idle" })
	a.accepted(second)
	a.untilIdle(sid)
	if llm.requestCount() != 1 {
		t.Fatalf("requests=%d", llm.requestCount())
	}
}
