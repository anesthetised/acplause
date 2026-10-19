package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

const planArgs = `{"entries":[{"content":"Implement <&>","priority":"high","status":"in_progress"},{"content":"Verify","priority":"low","status":"pending"}]}`

func TestPlanToolLifecycle(t *testing.T) {
	model := make(scriptedModel, 4)
	model.callTool("first", "update_plan", planArgs)
	results := make(chan llm.Message, 2)
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		results <- req.Messages[len(req.Messages)-1]
		return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "second", Function: llm.FunctionCall{Name: "update_plan", Arguments: `{"planId":"model-chosen","entries":[{"content":"Verify","priority":"medium","status":"completed"}]}`}}}, FinishReason: llm.FinishToolCalls}, nil
	}
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		results <- req.Messages[len(req.Messages)-1]
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}
	c := startAgent(t, t.TempDir(), model, nil)
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) {
		t.Error("plan requested permission")
		return acp.OutcomeSelected, optionReject
	}
	c.mu.Unlock()
	c.initialize() // No optional capabilities are required.
	sid := c.newSession(t.TempDir())
	c.selectModel(sid, "m")
	c.agent.mu.Lock()
	s := c.agent.sessions[sid]
	c.agent.mu.Unlock()
	req := s.request()
	if !slices.ContainsFunc(req.Tools, func(tool llm.Tool) bool { return tool.Function.Name == "update_plan" }) {
		t.Fatal("update_plan not advertised")
	}
	c.await(c.prompt(sid, "work"))
	updates := c.untilIdle(sid)
	var plans []acp.PlanUpdate
	var statuses []string
	for _, n := range updates {
		switch n.Update.Kind {
		case "plan_update":
			var plan acp.PlanUpdate
			if err := json.Unmarshal(n.Update.Raw, &plan); err != nil {
				t.Fatal(err)
			}
			plans = append(plans, plan)
		case "tool_call_update":
			statuses = append(statuses, n.Update.Status)
		}
	}
	if len(plans) != 2 || plans[0].Plan.PlanID == "" || plans[0].Plan.PlanID == "model-chosen" || plans[0].Plan.PlanID != plans[1].Plan.PlanID || len(plans[1].Plan.Entries) != 1 || plans[1].Plan.Entries[0].Status != "completed" {
		t.Fatalf("plan replacements: %+v", plans)
	}
	if !slices.Equal(statuses, []string{"in_progress", "completed", "in_progress", "completed"}) {
		t.Fatalf("tool lifecycle: %v", statuses)
	}
	for _, id := range []string{"first", "second"} {
		result := <-results
		var output struct{ PlanID string }
		if err := json.Unmarshal([]byte(result.Content), &output); err != nil || output.PlanID != plans[0].Plan.PlanID || result.Role != llm.RoleTool || result.ToolCallID != id {
			t.Fatalf("model result: %+v, %v", result, err)
		}
	}
	model.callTool("third", "update_plan", planArgs)
	model.reply("Unfinished", llm.FinishStop, nil)
	c.await(c.prompt(sid, "next turn"))
	next := c.untilIdle(sid)
	var secondID string
	for _, n := range next {
		if n.Update.Kind == "plan_update" {
			var plan acp.PlanUpdate
			if err := json.Unmarshal(n.Update.Raw, &plan); err != nil {
				t.Fatal(err)
			}
			secondID = plan.Plan.PlanID
		}
	}
	if secondID == "" || secondID == plans[0].Plan.PlanID {
		t.Fatalf("new turn reused plan ID: %q", secondID)
	}
	c.ok(acp.MethodSessionClose, map[string]any{"sessionId": sid}, nil)
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": s.cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	var replay []acp.PlanUpdate
	for _, n := range c.untilIdle(sid) {
		if n.Update.Kind == "plan_update" {
			var plan acp.PlanUpdate
			if err := json.Unmarshal(n.Update.Raw, &plan); err != nil {
				t.Fatal(err)
			}
			replay = append(replay, plan)
		}
	}
	if len(replay) != 2 || replay[0].Plan.PlanID != plans[0].Plan.PlanID || len(replay[0].Plan.Entries) != 1 || replay[1].Plan.PlanID != secondID || replay[1].Plan.Entries[0].Status != "in_progress" {
		t.Fatalf("persisted plan tool snapshots: %+v", replay)
	}
}

func TestPlanToolInvalidArguments(t *testing.T) {
	for _, args := range []string{
		"{", "null", "[]", `{}`, `{"entries":null}`, `{"entries":[]}`,
		strings.Replace(planArgs, `"high"`, `null`, 1),
		strings.Replace(planArgs, `"pending"`, `"in_progress"`, 1),
		strings.Replace(planArgs, `"Verify"`, `false`, 1),
	} {
		t.Run(args, func(t *testing.T) {
			model := make(scriptedModel, 3)
			model.callTool("valid", "update_plan", planArgs)
			model.callTool("invalid", "update_plan", args)
			result := make(chan llm.Message, 1)
			model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				result <- req.Messages[len(req.Messages)-1]
				return &llm.Completion{FinishReason: llm.FinishStop}, nil
			}
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			c.await(c.prompt(sid, "work"))
			updates := c.untilIdle(sid)
			plans := 0
			var statuses []string
			for _, n := range updates {
				if n.Update.Kind == "plan_update" {
					plans++
				}
				if n.Update.Kind == "tool_call_update" {
					statuses = append(statuses, n.Update.Status)
				}
			}
			last := <-result
			if plans != 1 || !slices.Equal(statuses, []string{"in_progress", "completed", "failed"}) || last.ToolCallID != "invalid" || !strings.HasPrefix(last.Content, "Error:") {
				t.Fatalf("invalid plan: plans=%d statuses=%v result=%+v", plans, statuses, last)
			}
		})
	}
}

func TestPlanToolInterruptedTurns(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancelled"}[cancelled], func(t *testing.T) {
			model := make(scriptedModel, 2)
			model.callTool("plan", "update_plan", planArgs)
			started := make(chan struct{})
			model <- func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				close(started)
				if cancelled {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, errors.New("provider failed")
			}
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			c.await(c.prompt(sid, "work"))
			if cancelled {
				c.waitFor(sid, func(n notification) bool { return n.Update.Kind == "plan_update" })
				<-started
				c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
			}
			want := "error"
			if cancelled {
				want = "cancelled"
			}
			if idle := lastIdle(c.untilIdle(sid)); idle.Update.StopReason != want {
				t.Fatalf("idle: %+v", idle)
			}
		})
	}
}

func TestPlanToolRevokedBeforePublish(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "lost"}[lost], func(t *testing.T) {
			s, out := testSession(t, nil)
			insertPlanPrompt(t, s)
			call, err := s.preparePlan(planArgs)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if lost {
				s.markLost()
			} else {
				cancel()
			}
			count := len(out)
			result := call.run(ctx)
			if !result.Failed || !strings.HasPrefix(result.Output, "Error:") || s.planID != "" || len(out) != count {
				t.Fatalf("revoked tool published: %+v", result)
			}
		})
	}
}
