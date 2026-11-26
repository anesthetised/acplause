package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

	// A live process still has its restored message indexed when a third
	// process takes over the idle session.
	c := start(t, llm, withDataDir(dir))
	c.initialize()
	c.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, nil)
	c.untilIdle(sid)
	if err := b.fails("session/prompt", params(sid, "stale retry"), -32603); err.Message != "session was resumed in another acplause process" {
		t.Fatalf("old owner retry: %+v", err)
	}
	if got, _ := c.accepted(c.request("session/prompt", params(sid, "current owner retry"))); got != original {
		t.Fatalf("current owner retry = %q, want %q", got, original)
	}
	if got := llm.requestCount(); got != 3 {
		t.Fatalf("ownership checks issued another model request: %d", got)
	}
	// Hold SQLite acquisition pending after b confirmed its cached
	// generation lost ownership. The prompt error must arrive before commit.
	db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE sessions SET title = title WHERE id = ?", sid); err != nil {
		t.Fatal(err)
	}
	resume := b.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd})
	b.sessionNotFound("session/prompt", params(sid, "before reacquisition"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if r := b.response(resume); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got, _ := b.accepted(b.request("session/prompt", params(sid, "after reacquisition"))); got != original {
		t.Fatalf("restored ID = %q, want %q", got, original)
	}
}

func TestKeyedCancellationBeforeAndAfterSettlement(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	queue, turn := make(chan struct{}), make(chan struct{})
	llm.enqueue(waitFor(queue, nil), waitFor(turn, nil))
	a, sid, _ := started(t, llm)
	a.accepted(a.prompt(sid, "hold the worker"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	params := map[string]any{"sessionId": sid, "idempotencyKey": "k", "prompt": []any{map[string]any{"type": "text", "text": "queued"}}}
	pair := func() []string {
		return []string{a.request("session/prompt", params), a.request("session/prompt", params)}
	}
	cancel := func(id string) { a.notify("$/cancel_request", map[string]any{"requestId": requestID(t, id)}) }
	for member := range 2 {
		ids := pair()
		cancel(ids[member])
		for _, id := range ids {
			if e := a.response(id).Error; e == nil || e.Code != -32800 {
				t.Fatalf("queued request %s, cancelled %s: %+v", id, ids[member], e)
			}
		}
	}
	ids := pair()
	a.ok("session/list", map[string]any{}, nil) // Both requests joined the queue.
	close(queue)
	a.untilIdle(sid)
	one, _ := a.accepted(ids[0])
	two, _ := a.accepted(ids[1])
	if one != two {
		t.Fatalf("joined requests received %q and %q", one, two)
	}
	for _, id := range ids {
		cancel(id)
	}
	a.ok("session/list", map[string]any{}, nil) // Both cancellations reached the reader.
	close(turn)
	updates := a.untilIdle(sid)
	echoes := filter(updates, "user_message")
	if len(echoes) != 1 || echoes[0].MessageID != one || updates[len(updates)-1].StopReason != "end_turn" || llm.requestCount() != 2 {
		t.Fatalf("settled turn after cancellation: %v, model calls %d", kinds(updates), llm.requestCount())
	}
}

func TestRetainedRetrySurvivesFailedReconfiguration(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("done"))
	a, sid, cwd := started(t, llm)
	prompt := func(key string) string {
		return a.request("session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]string{"type": "text", "text": "ask"}}, "idempotencyKey": key})
	}
	original, _ := a.accepted(prompt("retained"))
	a.untilIdle(sid)
	entered, release := make(chan struct{}), make(chan struct{})
	started := sync.OnceFunc(func() { close(entered) })
	unblock := sync.OnceFunc(func() { close(release) })
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer peer.Close()
	defer unblock()
	resume := a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{map[string]string{"type": "http", "name": "unavailable", "url": peer.URL}}})
	select {
	case <-entered:
	case <-time.After(timeout):
		t.Fatal("MCP setup did not start")
	}
	retry, fresh := prompt("retained"), prompt("new")
	a.fails("_test/barrier", nil, -32601)
	unblock()
	for _, id := range []string{resume, fresh} {
		if r := a.response(id); r.Error == nil || r.Error.Code != -32602 {
			t.Fatalf("request %s: %+v", id, r)
		}
	}
	if got, _ := a.accepted(retry); got != original || llm.requestCount() != 1 {
		t.Fatalf("retained retry = %q, want %q; model calls = %d", got, original, llm.requestCount())
	}
}
