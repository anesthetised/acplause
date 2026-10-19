package e2e

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // exercises persistence across processes
)

// Two agents sharing a data directory stand in for two editor windows.

func setModel(a *agent, sid string) {
	a.t.Helper()
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
}

func TestSessionsSurviveRestart(t *testing.T) {
	t.Parallel()
	dir, cwd := t.TempDir(), t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(reply("Paris."), reply("Berlin."))

	first := start(t, llm, withDataDir(dir))
	first.initialize()
	sid := first.newSession(cwd).SessionID
	setModel(first, sid)
	first.accepted(first.prompt(sid, "Capital of France?"))
	first.untilIdle(sid)
	first.stop()

	second := start(t, llm, withDataDir(dir))
	second.initialize()
	var list listResult
	second.ok("session/list", map[string]any{}, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != sid || list.Sessions[0].Title != "Capital of France?" {
		t.Fatalf("session/list after restart: %+v", list)
	}
	var resumed struct {
		ConfigOptions []configOption `json:"configOptions"`
	}
	second.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, &resumed)
	replay := second.untilIdle(sid)
	if got := kinds(replay); !slices.Equal(got, []string{"user_message", "agent_message", "state_update idle"}) || replay[1].text() != "Paris." {
		t.Fatalf("replay after restart: %v", got)
	}
	if resumed.ConfigOptions[0].CurrentValue != "test/model" {
		t.Fatalf("model after restart: %+v", resumed.ConfigOptions)
	}

	second.accepted(second.prompt(sid, "And Germany?"))
	second.untilIdle(sid)
	var contents []string
	for _, m := range llm.request(1).Messages[1:] {
		contents = append(contents, m["content"].(string))
	}
	if !slices.Equal(contents, []string{"Capital of France?", "Paris.", "And Germany?"}) {
		t.Fatalf("history after restart: %q", contents)
	}
}

func TestIdleSessionIsTakenOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(reply("one"), reply("two"))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	a.accepted(a.prompt(sid, "first"))
	a.untilIdle(sid)

	b := start(t, llm, withDataDir(dir))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, nil)
	if snapshot := b.nextUpdate(sid); snapshot.State != "idle" {
		t.Fatalf("resume snapshot: %+v", snapshot)
	}

	// The first window learns it lost the session when it next prompts.
	resp := a.response(a.prompt(sid, "from a"))
	if resp.Error == nil || resp.Error.Code != -32603 || !strings.Contains(resp.Error.Message, "resumed in another acplause process") {
		t.Fatalf("prompt to a lost session: %+v", resp.Error)
	}
	b.accepted(b.prompt(sid, "from b"))
	if idle := b.untilIdle(sid); idle[len(idle)-1].StopReason != "end_turn" {
		t.Fatalf("turn after takeover: %v", kinds(idle))
	}
}

func TestBusySessionIsNotTakenOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	release := make(chan struct{})
	llm := newMockLLM(t)
	llm.enqueue(waitFor(release, nil))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	a.accepted(a.prompt(sid, "work"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })

	b := start(t, llm, withDataDir(dir))
	b.initialize()
	if e := b.fails("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, -32603); !strings.Contains(e.Message, "busy") {
		t.Fatalf("resume of a busy session: %+v", e)
	}
	if e := b.fails("session/resume", map[string]any{"sessionId": "nope", "cwd": cwd}, -32002); e == nil {
		t.Fatal("unknown session")
	}
	close(release)
	if idle := a.untilIdle(sid); idle[len(idle)-1].StopReason != "end_turn" {
		t.Fatalf("owner's turn: %v", kinds(idle))
	}
}

func TestContendedPromptCanBeCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, sid, cwd := started(t, newMockLLM(t), withDataDir(dir))
	db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db")+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	prompt := a.prompt(sid, "cancel before insertion")
	// This response must arrive while the write lock is still held.
	setModel(a, sid)
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	// Fence the cancellation on the protocol read loop before unlocking.
	setModel(a, sid)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if resp := a.response(prompt); resp.Error == nil || resp.Error.Code != -32800 {
		t.Fatalf("cancelled prompt: %+v", resp)
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	if replay := a.untilIdle(sid); len(replay) != 1 {
		t.Fatalf("cancelled prompt was inserted: %v", kinds(replay))
	}
}
