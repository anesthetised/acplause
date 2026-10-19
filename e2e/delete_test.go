package e2e

import (
	"strings"
	"testing"
	"time"
)

func TestDeleteSession(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t))
	a.fails("session/delete", map[string]any{"sessionId": "unknown"}, -32600)
	a.initialize()
	a.fails("session/delete", map[string]any{}, -32602)
	a.ok("session/delete", map[string]any{"sessionId": "unknown", "future": true}, nil)

	cwd := t.TempDir()
	for _, closeFirst := range []bool{false, true} {
		sid := a.newSession(cwd).SessionID
		if closeFirst {
			a.ok("session/close", map[string]any{"sessionId": sid}, nil)
		}
		resp := a.ok("session/delete", map[string]any{"sessionId": sid}, nil)
		assertJSON(t, resp.Result, `{}`)
		a.ok("session/delete", map[string]any{"sessionId": sid}, nil)
		a.fails("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, -32002)
		var list listResult
		a.ok("session/list", map[string]any{}, &list)
		if len(list.Sessions) != 0 {
			t.Fatalf("deleted session still listed: %+v", list)
		}
	}
}

func TestDeleteCancelsLocalWork(t *testing.T) {
	t.Parallel()
	aborted := make(chan struct{})
	llm := newMockLLM(t)
	llm.enqueue(waitFor(make(chan struct{}), aborted))
	a, sid, cwd := started(t, llm)
	a.accepted(a.prompt(sid, "working"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	queued := a.prompt(sid, "queued")
	resp := a.ok("session/delete", map[string]any{"sessionId": sid}, nil)
	if rejected := a.response(queued); rejected.Error == nil || rejected.Error.Code != -32800 {
		t.Fatalf("queued prompt: %+v", rejected)
	}
	idle := a.untilIdle(sid)
	if last := idle[len(idle)-1]; last.StopReason != "cancelled" || last.seq > resp.seq {
		t.Fatalf("delete did not wait for cancellation: %+v", last)
	}
	select {
	case <-aborted:
	case <-time.After(timeout):
		t.Fatal("delete left the model request running")
	}
	a.fails("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, -32002)
	a.expectQuiet(func(m *message) bool { return m.Method == "session/update" && sessionOf(m) == sid })
}

func TestDeleteSessionInAnotherProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(waitFor(make(chan struct{}), nil))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	a.accepted(a.prompt(sid, "work"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	b := start(t, llm, withDataDir(dir))
	b.initialize()
	if e := b.fails("session/delete", map[string]any{"sessionId": sid}, -32603); !strings.Contains(e.Message, "busy") {
		t.Fatalf("busy session: %+v", e)
	}
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	a.untilIdle(sid)
	// Close waits for the final save; resume leaves an idle owner in a.
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, nil)
	a.untilIdle(sid)
	b.ok("session/delete", map[string]any{"sessionId": sid}, nil)
	if resp := a.response(a.prompt(sid, "deleted elsewhere")); resp.Error == nil || resp.Error.Code != -32603 {
		t.Fatalf("old owner accepted prompt: %+v", resp)
	}
	var list listResult
	a.ok("session/list", map[string]any{}, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("deleted session recreated: %+v", list)
	}
}
