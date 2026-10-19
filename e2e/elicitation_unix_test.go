//go:build unix

package e2e

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestElicitationCrashDoesNotReissue(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("ask_user", formArgs), reply("Recovered."))
	a, sid, cwd := startedForms(t, llm, withDataDir(data))
	a.accepted(a.prompt(sid, "ask"))
	q := a.elicitationRequest()
	a.untilState(sid, "requires_action")
	a.kill()
	db, err := sql.Open("sqlite", filepath.Join(data, "acplause.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE sessions SET lease_until = 0 WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	b := start(t, llm, withDataDir(data))
	b.initialize() // capabilities must be negotiated afresh
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	calls := filter(b.untilIdle(sid), "tool_call_update")
	if len(calls) != 1 || calls[0].Name != "ask_user" || calls[0].Status != "failed" || !strings.Contains(calls[0].text(), "exited before") {
		t.Fatalf("recovery: %+v", calls)
	}
	b.answer(q, formAnswer("accept")) // a reply from the old connection cannot resume work
	b.accepted(b.prompt(sid, "continue"))
	b.untilIdle(sid)
	req := llm.request(1)
	results := 0
	for _, m := range req.Messages {
		if m["role"] == "tool" && m["tool_call_id"] == "call_ask_user" {
			results++
			if !strings.Contains(m["content"].(string), "exited before") {
				t.Fatalf("result: %+v", m)
			}
		}
	}
	if results != 1 {
		t.Fatalf("matching results=%d", results)
	}
	for _, tool := range req.Tools {
		if tool["function"].(map[string]any)["name"] == "ask_user" {
			t.Fatal("old form capability persisted")
		}
	}
	b.expectNoElicitation()
}
