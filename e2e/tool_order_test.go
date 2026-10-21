package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func TestToolCallsExecuteInIndexOrder(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(func(s *stream) {
		s.toolCall(1, "edit", "edit", `{"path":"ordered.txt","old_text":"before","new_text":"after"}`)
		s.toolCall(0, "create", "write_file", `{"path":"ordered.txt","content":"before"}`)
		s.finish("tool_calls")
	}, reply("Done."))
	a, sid, cwd := started(t, llm)
	a.accepted(a.prompt(sid, "create then edit"))
	updates := a.untilIdle(sid)
	var completed []string
	for _, u := range filter(updates, "tool_call_update") {
		if u.Status == "failed" {
			t.Fatalf("tool failed: %+v", u)
		}
		if u.Status == "completed" {
			completed = append(completed, u.ToolCallID)
		}
	}
	if len(completed) != 2 {
		t.Fatalf("completion order: %v", completed)
	}
	msgs := llm.request(1).Messages
	if len(msgs) != 5 || msgs[3]["tool_call_id"] != "create" || msgs[4]["tool_call_id"] != "edit" {
		t.Fatalf("tool result order: %v", msgs)
	}
	if b, err := os.ReadFile(filepath.Join(cwd, "ordered.txt")); err != nil || string(b) != "after" {
		t.Fatalf("file after ordered tools: %q, %v", b, err)
	}
}
