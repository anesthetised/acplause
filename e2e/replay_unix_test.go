//go:build unix

package e2e

import (
	"path/filepath"
	"testing"
)

func TestResumeReplaysRunningToolAndCancellation(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("bash", `{"command":"echo ready > ready; sleep 60"}`))
	a, sid, cwd := started(t, llm)
	a.accepted(a.prompt(sid, "run"))
	start := a.toolStarted(sid)
	waitForFile(t, filepath.Join(cwd, "ready"))
	params := map[string]any{
		"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{t.TempDir()},
		"replayFrom": map[string]any{"type": "start"},
	}
	resp := a.ok("session/resume", params, nil)
	replay := a.updateWhere(sid, func(u update) bool { return u.is("tool_call_update") })
	if replay.ToolCallID != start.ToolCallID || replay.Status != "in_progress" || replay.Name != "bash" || replay.seq > resp.seq {
		t.Fatalf("running tool replay: %+v", replay)
	}
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	updates := a.untilIdle(sid)
	calls := filter(updates, "tool_call_update")
	if len(calls) != 1 || calls[0].Status != "cancelled" || updates[len(updates)-1].StopReason != "cancelled" {
		t.Fatalf("cancellation after resume: %v", updates)
	}
	a.ok("session/resume", params, nil)
	calls = filter(a.untilIdle(sid), "tool_call_update")
	if len(calls) != 1 || calls[0].ToolCallID != start.ToolCallID || calls[0].Status != "cancelled" || calls[0].Name != "bash" {
		t.Fatalf("final tool replay: %v", calls)
	}
}
