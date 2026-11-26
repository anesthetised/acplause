//go:build unix

package e2e

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPlanRemovalSurvivesCrashAfterNotification(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("update_plan", initialPlan), waitFor(make(chan struct{}), nil))
	a, sid, cwd := started(t, llm, withDataDir(data))
	a.accepted(a.prompt(sid, "work"))
	llm.request(1)
	plan := a.nextPlan(sid)

	db, err := sql.Open("sqlite", filepath.Join(data, "acplause.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Prevent the post-idle flush from making the removal durable before
	// the test can kill the process.
	if _, err := db.Exec(`CREATE TRIGGER reject_idle_save BEFORE UPDATE OF state ON sessions
		WHEN NEW.state = 'idle' BEGIN SELECT RAISE(ABORT, 'refused idle save'); END`); err != nil {
		t.Fatal(err)
	}
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	removed := a.updateWhere(sid, func(u update) bool { return u.is("plan_removed") })
	if removed.PlanID != plan.Plan.PlanID {
		t.Fatalf("removed plan %q, want %q", removed.PlanID, plan.Plan.PlanID)
	}
	a.kill()
	if _, err := db.Exec("DROP TRIGGER reject_idle_save"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET lease_until = 0 WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}

	b := start(t, llm, withDataDir(data))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := b.untilIdle(sid)
	markers := filter(replay, "replay_complete")
	if len(markers) != 1 || len(filter(replay, "plan_update")) != 0 {
		t.Fatalf("replay resurrected the removed plan: %+v", replay)
	}
	checkPlanRemoval(t, replay, plan.Plan.PlanID, markers[0].seq, true)
}
