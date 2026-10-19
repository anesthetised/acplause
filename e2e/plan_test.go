package e2e

import (
	"encoding/json"
	"os"
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
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "failed"}[failure], func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("update_plan", initialPlan))
			if failure {
				llm.enqueue(func(s *stream) { s.finish("error") })
			} else {
				llm.enqueue(waitFor(make(chan struct{}), nil))
			}
			a, sid, cwd := started(t, llm, askingUser)
			a.accepted(a.prompt(sid, "work"))
			llm.request(1) // The plan and its matching tool result precede this round.
			if !failure {
				a.notify("session/cancel", map[string]any{"sessionId": sid})
			}
			updates := a.untilIdle(sid)
			want := "cancelled"
			if failure {
				want = "error"
			}
			plans := filter(updates, "plan_update")
			if updates[len(updates)-1].StopReason != want || len(plans) != 1 || plans[0].Plan.Entries[0].Status != "in_progress" {
				t.Fatalf("interrupted plan: %+v", updates)
			}
			a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := filter(a.untilIdle(sid), "plan_update")
			if len(replay) != 1 || !reflect.DeepEqual(replay[0].Plan, plans[0].Plan) {
				t.Fatalf("interrupted plan changed on replay: %+v", replay)
			}
			a.expectQuiet(func(m *message) bool { return m.Method == "session/update" && decodeUpdate(t, m).State == "idle" })
		})
	}
}
