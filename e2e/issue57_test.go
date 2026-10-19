package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssue57SearchExactFileLimit(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("search_code", `{"pattern":"needle","file_glob":"*.txt"}`), reply("Found."))
	a, sid, cwd := started(t, llm)
	content := "needle" + strings.Repeat("x", (1<<20)-len("needle"))
	if err := os.WriteFile(filepath.Join(cwd, "match.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	a.accepted(a.prompt(sid, "find the boundary match"))
	updates := a.untilIdle(sid)
	calls := filter(updates, "tool_call_update")
	if len(calls) != 2 || calls[1].Status != "completed" {
		t.Fatalf("search updates: %+v", calls)
	}
	want := "match.txt:1:" + "needle" + strings.Repeat("x", 300-len("needle")) + "…\n"
	if got := calls[1].text(); got != want {
		t.Fatalf("search result: got %q, want clipped result %q", got, want)
	}
	result := llm.request(1).Messages
	if got := result[len(result)-1]["content"]; got != want {
		t.Fatalf("search history result: got %v, want %q", got, want)
	}
}
