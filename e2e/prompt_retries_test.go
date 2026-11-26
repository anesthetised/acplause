package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Discard the first process and its unread prompt response. The only ID the
// client can match against after reconnecting comes from the user_message echo.
func TestPromptRetryAfterLostResponseAndCompaction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t, compactionModels)
	llm.enqueue(reply(strings.Repeat("old answer ", 100)), reply("Summary of the first exchange."), reply("Independent session."))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	const key = "PRIVATE-retry-identity"
	params := func(session, text string) map[string]any {
		return map[string]any{"sessionId": session, "idempotencyKey": key, "prompt": []any{map[string]any{"type": "text", "text": text}}}
	}
	private := func(raw []byte) {
		t.Helper()
		if bytes.Contains(raw, []byte("idempotencyKey")) || bytes.Contains(raw, []byte(key)) {
			t.Fatal("private prompt identity escaped")
		}
	}
	untilIdle := func(a *agent) []update {
		t.Helper()
		var updates []update
		for {
			msg := a.take(func(m *message) bool { return m.Method == "session/update" && sessionOf(m) == sid })
			private(msg.Params)
			u := decodeUpdate(t, msg)
			updates = append(updates, u)
			if u.State == "idle" {
				return updates
			}
		}
	}
	a.request("session/prompt", params(sid, "first exchange"))
	echoes := filter(untilIdle(a), "user_message")
	if len(echoes) != 1 || echoes[0].MessageID == "" {
		t.Fatalf("original echoes: %+v", echoes)
	}
	original := echoes[0].MessageID
	command, _ := a.accepted(a.prompt(sid, "/compact"))
	compactions := filter(untilIdle(a), "compaction_update")
	if len(compactions) != 2 || compactions[1].Status != "completed" {
		t.Fatalf("compaction: %+v", compactions)
	}
	a.stop()

	b := start(t, llm, withDataDir(dir))
	b.initialize()
	private(b.ok("session/list", map[string]any{}, nil).Result)
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replayed := filter(untilIdle(b), "user_message")
	if len(replayed) != 2 || replayed[0].MessageID != original || replayed[1].MessageID != command {
		t.Fatalf("replayed messages: %+v", replayed)
	}
	got, resp := b.accepted(b.request("session/prompt", params(sid, "different retry content")))
	private(resp.Result)
	if got != original {
		t.Fatalf("retry = %q, original = %q", got, original)
	}
	b.expectQuiet(func(m *message) bool { return m.Method == "session/update" && sessionOf(m) == sid })
	if got := llm.requestCount(); got != 2 {
		t.Fatalf("requests including compaction = %d", got)
	}

	other := b.newSession(t.TempDir()).SessionID
	setModel(b, other)
	independent, _ := b.accepted(b.request("session/prompt", params(other, "other session")))
	if independent == original || len(filter(b.untilIdle(other), "user_message")) != 1 {
		t.Fatal("key crossed sessions")
	}
	for i := range 3 {
		raw, err := json.Marshal(llm.request(i))
		if err != nil {
			t.Fatal(err)
		}
		private(raw)
	}
}
