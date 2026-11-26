package e2e

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type planEntry struct {
	Content, Priority, Status string
}

type planSnapshot struct {
	Type, PlanID string
	Entries      []planEntry
}

const initialPlan = `{"entries":[{"content":"Implement <&>","priority":"high","status":"in_progress"},{"content":"Verify","priority":"low","status":"pending"}]}`
const finishedPlan = `{"entries":[{"content":"Verify <&>","priority":"medium","status":"completed"}]}`

func TestPlanUpdatesAndRestart(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("update_plan", initialPlan), callTool("update_plan", finishedPlan), callTool("update_plan", finishedPlan), reply("Done."))
	a, sid, cwd := started(t, llm, withDataDir(data), askingUser)
	a.accepted(a.prompt(sid, "work"))
	updates := a.untilIdle(sid)
	plans := filter(updates, "plan_update")
	if len(plans) != 3 || plans[0].Plan == nil || plans[0].Plan.Type != "items" || plans[0].Plan.PlanID == "" || len(plans[0].Plan.Entries) != 2 {
		t.Fatalf("initial plan: %+v", plans)
	}
	firstID := plans[0].Plan.PlanID
	for _, u := range plans[1:] {
		if u.Plan == nil || u.Plan.PlanID != firstID || !slices.Equal(u.Plan.Entries, []planEntry{{"Verify <&>", "medium", "completed"}}) {
			t.Fatalf("full replacement: %+v", u.Plan)
		}
	}
	calls := filter(updates, "tool_call_update")
	if len(calls) != 6 {
		t.Fatalf("tool lifecycle: %+v", calls)
	}
	for i := range 3 {
		if calls[i*2].Status != "in_progress" || calls[i*2+1].Status != "completed" || calls[i*2].ToolCallID != calls[i*2+1].ToolCallID || calls[i*2].Kind != "other" {
			t.Fatalf("plan tool lifecycle: %+v", calls)
		}
		req := llm.request(i + 1)
		result := req.Messages[len(req.Messages)-1]
		var output struct{ PlanID string }
		if err := json.Unmarshal([]byte(result["content"].(string)), &output); err != nil || output.PlanID != firstID || result["role"] != "tool" || result["tool_call_id"] != "call_update_plan" {
			t.Fatalf("model tool result: %+v, %v", result, err)
		}
	}
	if !slices.ContainsFunc(llm.request(0).Tools, func(tool map[string]any) bool { return tool["function"].(map[string]any)["name"] == "update_plan" }) {
		t.Fatal("plan tool missing without optional client capabilities")
	}

	llm.enqueue(callTool("update_plan", initialPlan), reply("Unfinished."))
	a.accepted(a.prompt(sid, "another turn"))
	second := filter(a.untilIdle(sid), "plan_update")
	if len(second) != 1 || second[0].Plan.PlanID == firstID || second[0].Plan.PlanID == "" {
		t.Fatalf("new turn identity: %+v", second)
	}
	if files, err := os.ReadDir(cwd); err != nil || len(files) != 0 {
		t.Fatalf("plan changed the workspace: %v, %v", files, err)
	}
	for _, m := range a.pending {
		if m.Method == "session/request_permission" {
			t.Fatal("plan requested permission")
		}
	}
	a.expectQuiet(func(m *message) bool { return m.Method == "session/request_permission" })
	a.stop()

	b := start(t, llm, withDataDir(data), askingUser)
	b.initialize()
	resp := b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := b.untilIdle(sid)
	replayed := filter(replay, "plan_update")
	if len(replayed) != 2 || !reflect.DeepEqual(replayed[0].Plan, plans[2].Plan) || !reflect.DeepEqual(replayed[1].Plan, second[0].Plan) || replayed[1].seq > resp.seq {
		t.Fatalf("restart snapshots: %+v", replayed)
	}
	users := filter(replay, "user_message")
	if len(users) != 2 || replayed[0].seq > users[1].seq || replayed[1].Plan.Entries[0].Status != "in_progress" {
		t.Fatalf("snapshot position or unfinished status changed: %+v", replay)
	}
	llm.enqueue(callTool("update_plan", finishedPlan), reply("Continued."))
	b.accepted(b.prompt(sid, "continue"))
	third := filter(b.untilIdle(sid), "plan_update")
	if len(third) != 1 || third[0].Plan.PlanID == firstID || third[0].Plan.PlanID == second[0].Plan.PlanID {
		t.Fatalf("restart reused a plan ID: %+v", third)
	}
}

func TestPlanInvalidArgumentsLeaveSnapshot(t *testing.T) {
	t.Parallel()
	for _, args := range []string{"{", "null", `{"entries":[]}`, strings.Replace(initialPlan, `"pending"`, `"in_progress"`, 1)} {
		t.Run(args, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("update_plan", initialPlan), callTool("update_plan", args), reply("Done."))
			a, sid, cwd := started(t, llm, askingUser)
			a.accepted(a.prompt(sid, "work"))
			updates := a.untilIdle(sid)
			plans, calls := filter(updates, "plan_update"), filter(updates, "tool_call_update")
			if len(plans) != 1 || len(calls) != 3 || calls[2].Name != "update_plan" || calls[2].Status != "failed" {
				t.Fatalf("invalid update: %+v", updates)
			}
			req := llm.request(2)
			result := req.Messages[len(req.Messages)-1]
			if result["role"] != "tool" || result["tool_call_id"] != "call_update_plan" || !strings.HasPrefix(result["content"].(string), "Error:") {
				t.Fatalf("invalid tool result: %+v", result)
			}
			a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := filter(a.untilIdle(sid), "plan_update")
			if len(replay) != 1 || !reflect.DeepEqual(replay[0].Plan, plans[0].Plan) {
				t.Fatalf("invalid update changed stored plan: %+v", replay)
			}
		})
	}
}

func TestPlanInterruptedTurns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, plan, stop string
		remove           bool
	}{
		{"explicit cancel", initialPlan, "cancelled", true},
		{"completed then cancelled", finishedPlan, "cancelled", false},
		{"close", initialPlan, "cancelled", false},
		{"provider failure", initialPlan, "error", false},
		{"disconnect", initialPlan, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := t.TempDir()
			llm := newMockLLM(t)
			llm.enqueue(callTool("update_plan", tc.plan))
			if tc.stop == "error" {
				llm.enqueue(func(s *stream) { s.finish("error") })
			} else {
				llm.enqueue(waitFor(make(chan struct{}), nil))
			}
			a, sid, cwd := started(t, llm, askingUser, withDataDir(data))
			a.accepted(a.prompt(sid, "work"))
			req := llm.request(1)
			count := 0
			for _, m := range req.Messages {
				if m["role"] == "tool" && m["tool_call_id"] == "call_update_plan" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("plan tool result count = %d, want 1", count)
			}
			plan := a.nextPlan(sid)
			switch tc.name {
			case "explicit cancel", "completed then cancelled":
				queued := a.prompt(sid, "withdraw queued prompt")
				a.notify("session/cancel", map[string]any{"sessionId": sid})
				a.notify("session/cancel", map[string]any{"sessionId": sid})
				if resp := a.response(queued); resp.Error == nil || resp.Error.Code != -32800 {
					t.Fatalf("queued prompt was not withdrawn: %+v", resp)
				}
			case "close":
				a.ok("session/close", map[string]any{"sessionId": sid}, nil)
			}
			if tc.stop != "" {
				updates := a.untilIdle(sid)
				idle := updates[len(updates)-1]
				if idle.StopReason != tc.stop {
					t.Fatalf("interrupted plan: %+v", updates)
				}
				checkPlanRemoval(t, updates, plan.Plan.PlanID, idle.seq, tc.remove)
				isFinal := func(m *message) bool {
					if m.Method != "session/update" || sessionOf(m) != sid {
						return false
					}
					u := decodeUpdate(t, m)
					return u.State == "idle" || u.is("plan_removed")
				}
				if slices.ContainsFunc(a.pending, isFinal) {
					t.Fatal("duplicate idle or removal already buffered")
				}
				a.expectQuiet(isFinal)
			}
			a.stop()

			b := start(t, llm, askingUser, withDataDir(data))
			b.initialize()
			resp := b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := b.untilIdle(sid)
			plans, markers := filter(replay, "plan_update"), filter(replay, "replay_complete")
			if len(markers) != 1 || markers[0].seq >= replay[len(replay)-1].seq || replay[len(replay)-1].seq >= resp.seq {
				t.Fatalf("replay marker, state and response out of order: %+v", replay)
			}
			checkPlanRemoval(t, replay, plan.Plan.PlanID, markers[0].seq, tc.remove)
			if tc.remove {
				if len(plans) != 0 {
					t.Fatalf("replay restored a removed plan: %+v", plans)
				}
			} else if len(plans) != 1 || !reflect.DeepEqual(plans[0].Plan, plan.Plan) || plans[0].seq >= markers[0].seq {
				t.Fatalf("interrupted plan changed on replay: %+v", replay)
			}
		})
	}
}

func TestLegacyEmptyPlanReplay(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("update_plan", initialPlan), reply("Done."))
	a, sid, cwd := started(t, llm, withDataDir(data))
	a.accepted(a.prompt(sid, "work"))
	plan := a.nextPlan(sid)
	a.untilIdle(sid)
	a.stop()

	db, err := sql.Open("sqlite", filepath.Join(data, "acplause.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// An older transcript row can contain an empty snapshot with no removal marker.
	if _, err := db.Exec(`UPDATE transcript SET data = json_set(data, '$.update.plan.entries', json('[]'))
		WHERE session_id = ? AND json_extract(data, '$.kind') = 'plan_update'`, sid); err != nil {
		t.Fatal(err)
	}
	b := start(t, llm, withDataDir(data))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	updates := b.untilIdle(sid)
	snapshots, removals := filter(updates, "plan_update"), filter(updates, "plan_removed")
	want := &planSnapshot{Type: "items", PlanID: plan.Plan.PlanID, Entries: []planEntry{}}
	if len(snapshots) != 1 || !reflect.DeepEqual(snapshots[0].Plan, want) || len(removals) != 0 {
		t.Fatalf("legacy empty snapshot changed: %+v", updates)
	}
}

func TestPlanRemovalSQLFailurePreservesPlan(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("update_plan", initialPlan), waitFor(make(chan struct{}), nil), reply("Continued."))
	a, sid, cwd := started(t, llm, withDataDir(data))
	a.accepted(a.prompt(sid, "work"))
	llm.request(1) // The running turn's original plan has been saved.
	plan := a.nextPlan(sid)

	db, err := sql.Open("sqlite", filepath.Join(data, "acplause.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_plan_removal BEFORE INSERT ON transcript
		WHEN json_extract(NEW.data, '$.planRemoved') = 1
		BEGIN SELECT RAISE(ABORT, 'refused plan removal'); END`); err != nil {
		t.Fatal(err)
	}

	a.notify("session/cancel", map[string]any{"sessionId": sid})
	updates := a.untilIdle(sid)
	idle := updates[len(updates)-1]
	checkPlanRemoval(t, updates, plan.Plan.PlanID, idle.seq, false)
	notices := filter(updates, "notice")
	if idle.StopReason != "cancelled" || len(notices) != 1 || notices[0].Severity != "warning" ||
		notices[0].Title != "Session changes could not be saved" {
		t.Fatalf("want a save warning and cancelled idle: %+v", updates)
	}

	checkReplay := func(a *agent) {
		t.Helper()
		a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
		updates := a.untilIdle(sid)
		plans := filter(updates, "plan_update")
		if len(plans) != 1 || !reflect.DeepEqual(plans[0].Plan, plan.Plan) ||
			len(filter(updates, "plan_removed")) != 0 || len(filter(updates, "notice")) != 0 {
			t.Fatalf("replay after failed removal: %+v", updates)
		}
	}
	checkReplay(a)
	if _, err := db.Exec("DROP TRIGGER reject_plan_removal"); err != nil {
		t.Fatal(err)
	}
	// Successful saves after recovery must not revive the rejected removal.
	a.accepted(a.prompt(sid, "continue"))
	updates = a.untilIdle(sid)
	if len(filter(updates, "plan_removed")) != 0 || updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("subsequent turn did not finish cleanly: %+v", updates)
	}
	a.stop()
	b := start(t, llm, withDataDir(data))
	b.initialize()
	checkReplay(b)
}

func TestPlanRemovalAfterReplayCut(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("update_plan", initialPlan), callTool("write_file", `{"path":"changed","content":"bad"}`), reply("Continued."))
	a, sid, cwd := started(t, llm, askingUser)
	a.accepted(a.prompt(sid, "work"))
	plan := a.nextPlan(sid)
	permission := a.permissionRequest()
	tool := a.updateWhere(sid, func(u update) bool { return u.is("tool_call_update") && u.Name == "write_file" })
	resume, finishSetup := gatedReplay(t, a, sid, cwd)
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	a.cancellation(permission)
	finishSetup()
	if resp := a.response(resume); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	updates := a.untilIdle(sid)
	markers, plans, removals := filter(updates, "replay_complete"), filter(updates, "plan_update"), filter(updates, "plan_removed")
	idle := updates[len(updates)-1]
	if len(markers) != 1 || len(plans) != 1 || !reflect.DeepEqual(plans[0].Plan, plan.Plan) || plans[0].seq >= markers[0].seq || idle.StopReason != "cancelled" {
		t.Fatalf("replay cut lost the pre-cancel plan or cancelled idle: %+v", updates)
	}
	checkPlanRemoval(t, updates, plan.Plan.PlanID, idle.seq, true)
	finals := slices.DeleteFunc(filter(updates, "tool_call_update"), func(u update) bool {
		return u.ToolCallID != tool.ToolCallID || u.Status != "cancelled"
	})
	if len(finals) != 1 || finals[0].seq <= markers[0].seq || finals[0].seq >= removals[0].seq {
		t.Fatalf("live tool result and removal out of order: %+v", updates)
	}
	a.accepted(a.prompt(sid, "continue"))
	count := 0
	for _, m := range llm.request(2).Messages {
		if m["role"] == "tool" && m["tool_call_id"] == "call_write_file" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cancelled tool result count = %d, want 1", count)
	}
}

func (a *agent) nextPlan(sid string) update {
	a.t.Helper()
	u := a.updateWhere(sid, func(u update) bool { return u.is("plan_update") })
	if u.Plan == nil || u.Plan.PlanID == "" {
		a.t.Fatalf("missing plan: %+v", u)
	}
	return u
}

func checkPlanRemoval(t *testing.T, updates []update, id string, before int, want bool) {
	t.Helper()
	removed := filter(updates, "plan_removed")
	if want {
		if len(removed) != 1 || removed[0].PlanID != id || removed[0].seq >= before {
			t.Fatalf("want one removal of %q before frame %d: %+v", id, before, updates)
		}
	} else if len(removed) != 0 {
		t.Fatalf("unexpected plan removal: %+v", removed)
	}
}
