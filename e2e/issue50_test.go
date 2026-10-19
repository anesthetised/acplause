package e2e

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssue50StoreRetakePreservesOtherOwnersTurn(t *testing.T) {
	t.Parallel()
	dir, cwd := t.TempDir(), t.TempDir()
	model := newMockLLM(t)
	model.enqueue(reply("A first answer"), reply("B unique answer"), reply("A third answer"))
	a := start(t, model, withDataDir(dir))
	a.initialize()
	id := a.newSession(cwd).SessionID
	setModel(a, id)
	// Close waits for the asynchronous model-selection save. Reopen before
	// installing the renewal trigger so only turn workers can still save.
	a.ok("session/close", map[string]any{"sessionId": id}, nil)
	a.ok("session/resume", map[string]any{"sessionId": id, "cwd": cwd}, nil)
	a.untilIdle(id)
	db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Fail only lease-only renewal writes, leaving Acquire's owner change
	// and Save untouched. Both processes retain their cached sessions even
	// if the test is paused across a renewal tick; no race against 10 seconds.
	if _, err := db.Exec(`CREATE TRIGGER issue50_hold_renewal BEFORE UPDATE OF lease_until ON sessions
		WHEN NEW.owner = OLD.owner AND NEW.owner != ''
		BEGIN SELECT RAISE(ABORT, 'issue50: renewal controlled by test'); END`); err != nil {
		t.Fatal(err)
	}
	waitSaved := func(answer string) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			var ready bool
			if err := db.QueryRow(`SELECT state = 'idle' AND EXISTS
				(SELECT 1 FROM history WHERE session_id = sessions.id AND instr(data, ?) > 0)
				FROM sessions WHERE id = ?`, answer, id).Scan(&ready); err != nil {
				t.Fatal(err)
			}
			if ready {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("idle turn containing %q was not saved", answer)
			}
			time.Sleep(time.Millisecond)
		}
	}
	a.accepted(a.prompt(id, "A first prompt"))
	a.untilIdle(id)
	waitSaved("A first answer")
	b := start(t, model, withDataDir(dir))
	b.initialize()
	resume := map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}
	b.ok("session/resume", resume, nil)
	b.untilIdle(id)
	b.accepted(b.prompt(id, "B unique prompt"))
	b.untilIdle(id)
	waitSaved("B unique answer")
	a.ok("session/resume", resume, nil)
	replay := a.untilIdle(id)
	var replayText strings.Builder
	for _, update := range replay {
		replayText.WriteString(update.text())
	}
	for _, marker := range []string{"B unique prompt", "B unique answer"} {
		if !strings.Contains(replayText.String(), marker) {
			t.Errorf("retaken transcript lost %q: %s", marker, replayText.String())
		}
	}
	a.accepted(a.prompt(id, "A third prompt"))
	a.untilIdle(id)
	waitSaved("A third answer")
	request, err := json.Marshal(model.request(2).Messages)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"B unique prompt", "B unique answer"} {
		if !strings.Contains(string(request), marker) {
			t.Errorf("retaken model history lost %q", marker)
		}
		for _, table := range []string{"history", "transcript"} {
			var present bool
			if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM "+table+" WHERE session_id = ? AND instr(data, ?) > 0)", id, marker).Scan(&present); err != nil {
				t.Fatal(err)
			}
			if !present {
				t.Errorf("retaking an idle session deleted %q from durable %s", marker, table)
			}
		}
	}
}
