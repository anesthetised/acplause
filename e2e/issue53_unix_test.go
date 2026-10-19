//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestIssue53SearchFIFOThenCancelAndClose(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			aborted := make(chan struct{})
			llm.enqueue(callTool("search_code", `{"pattern":"needle","file_glob":"*.txt"}`), waitFor(nil, aborted))
			a, sid, cwd := started(t, llm)
			if err := os.WriteFile(filepath.Join(cwd, "match.txt"), []byte("needle\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(cwd, "pipe.txt"), 0o600); err != nil {
				t.Fatal(err)
			}
			a.accepted(a.prompt(sid, "search the workspace"))
			start := a.updateWhere(sid, func(u update) bool { return u.is("tool_call_update") && u.Status == "in_progress" })
			done := a.updateWhere(sid, func(u update) bool { return u.is("tool_call_update") && u.Status == "completed" })
			if start.Name != "search_code" || done.ToolCallID != start.ToolCallID || done.text() != "match.txt:1:needle\n" {
				t.Fatalf("search result: name=%q id=%q text=%q", start.Name, done.ToolCallID, done.text())
			}
			req := llm.request(1)
			if result := req.Messages[len(req.Messages)-1]; result["role"] != "tool" || result["content"] != done.text() {
				t.Fatalf("search history: %v", result)
			}
			// The FIFO has no writer. Search must finish before the next model round;
			// cancellation and close must still settle that turn normally.
			a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
			if action == "cancel" {
				a.notify("session/cancel", map[string]any{"sessionId": sid})
			} else {
				resp := a.call("session/close", map[string]any{"sessionId": sid})
				if resp.Error != nil {
					t.Fatalf("close: %+v", resp.Error)
				}
			}
			updates := a.untilIdle(sid)
			if idle := updates[len(updates)-1]; idle.StopReason != "cancelled" {
				t.Fatalf("search turn cancellation: %+v", idle)
			}
			waitClosed(t, aborted, "search turn model request was not aborted")
			if action == "cancel" {
				a.ok("session/close", map[string]any{"sessionId": sid}, nil)
			}
		})
	}
}
