package agent

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

func TestAgentLimitDefaultsAndZero(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     Config
		rounds  int
		timeout time.Duration
	}{
		{"defaults", Config{}, 200, time.Hour},
		{"unlimited", Config{MaxRounds: new(0), MCPTimeout: new(time.Duration(0))}, 0, 0},
		{"explicit", Config{MaxRounds: new(2), MCPTimeout: new(time.Minute)}, 2, time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg, nil, nil, func(context.Context) ([]llm.Model, error) {
				return []llm.Model{{ID: "m"}}, nil
			})
			defer a.Shutdown()
			if a.deps.maxRounds != tt.rounds || a.deps.mcpTimeout != tt.timeout {
				t.Fatalf("rounds=%d timeout=%v", a.deps.maxRounds, a.deps.mcpTimeout)
			}
		})
	}
}

func TestTurnRequestBudgets(t *testing.T) {
	for _, limit := range []int{0, 2} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			calls := 0
			s, out := testSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
				calls++
				if calls <= 4 {
					return &llm.Completion{ToolCalls: callMessage("list").ToolCalls, FinishReason: llm.FinishToolCalls}, nil
				}
				return &llm.Completion{Content: "done", FinishReason: llm.FinishStop}, nil
			}))
			s.deps.maxRounds = limit
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("work")})
			updates := out.untilIdle(t)
			idle := updates[len(updates)-1].(acp.StateUpdate)
			wantCalls, wantStop := 5, acp.StopEndTurn
			if limit > 0 {
				wantCalls, wantStop = limit, acp.StopMaxTurnRequests
			}
			if calls != wantCalls || idle.StopReason != wantStop {
				t.Fatalf("calls=%d idle=%+v", calls, idle)
			}
		})
	}
}
