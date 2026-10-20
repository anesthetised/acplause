package agent

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

type budgetClient struct {
	recorder
	permissions atomic.Int32
}

func (c *budgetClient) Call(context.Context, string, any, any) error {
	c.permissions.Add(1)
	return nil
}

func budgetSession(t *testing.T, model Model, tokens uint64, usd float64) (*Session, *budgetClient) {
	t.Helper()
	s, out := testSession(t, model)
	c := &budgetClient{recorder: out}
	s.deps.out, s.deps.log = c, slog.New(slog.DiscardHandler)
	s.deps.maxTurnTokens, s.deps.maxTurnCostUSD = tokens, usd
	s.deps.maxRounds = 0
	return s, c
}

func assertBudgetStop(t *testing.T, updates []acp.Update, reason string) {
	t.Helper()
	idles := 0
	for _, u := range updates {
		if state, ok := u.(acp.StateUpdate); ok {
			if state.State == acp.StateRequiresAction {
				t.Fatal("budget stop started permission/form waiting")
			}
			if state.State == acp.StateIdle {
				idles++
				if state.StopReason != acp.StopError || state.Error == nil || state.Error.Code != jsonrpc.CodeInternalError || !strings.Contains(state.Error.Message, reason) {
					t.Fatalf("budget stop: %+v", state)
				}
			}
		}
	}
	if idles != 1 {
		t.Fatalf("budget idles=%d", idles)
	}
}

func TestTurnBudgetStopsSiblingToolsRetainsOutputAndResets(t *testing.T) {
	for _, limits := range []struct {
		name   string
		tokens uint64
		cost   float64
	}{{"tokens", 10, 0}, {"USD", 0, 0.5}, {"joint", 10, 0.5}} {
		t.Run(limits.name, func(t *testing.T) {
			requests := make(chan llm.Request, 2)
			s, out := budgetSession(t, fakeModel(func(_ context.Context, req llm.Request, delta func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				if len(requests) == 1 {
					delta(llm.Delta{Kind: llm.DeltaText, Text: "Retained answer."})
					return &llm.Completion{Content: "Retained answer.", Usage: budgetUsage(10, 0.5), ToolCalls: []llm.ToolCall{
						{ID: "write", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}},
						{ID: "shell", Function: llm.FunctionCall{Name: "bash", Arguments: `{"command":"touch changed"}`}},
					}}, nil
				}
				assertToolPairs(t, req.Messages)
				return &llm.Completion{Content: "Next turn.", FinishReason: llm.FinishStop, Usage: budgetUsage(1, 0.01)}, nil
			}), limits.tokens, limits.cost)
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("do work")})
			updates := out.untilIdle(t)
			assertBudgetStop(t, updates, "budget reached")
			failed := 0
			for _, u := range updates {
				if call, ok := u.(acp.ToolCallUpdate); ok {
					if call.Status != acp.ToolFailed || !strings.Contains(string(call.RawOutput), "budget reached") {
						t.Fatalf("unexecuted call: %+v", call)
					}
					failed++
				}
			}
			if failed != 2 || out.permissions.Load() != 0 || len(requests) != 1 {
				t.Fatalf("budget allowed further work: failed=%d permissions=%d requests=%d", failed, out.permissions.Load(), len(requests))
			}
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("continue")})
			next := out.untilIdle(t)
			if idle := next[len(next)-1].(acp.StateUpdate); idle.StopReason != acp.StopEndTurn {
				t.Fatalf("budget did not reset: %+v", idle)
			}
			<-requests
			req := <-requests
			if req.Messages[2].Content != "Retained answer." || req.Messages[3].ToolCallID != "write" || req.Messages[4].ToolCallID != "shell" {
				t.Fatalf("budget discarded output/history: %+v", req.Messages)
			}
			if _, err := os.Stat(filepath.Join(s.cwd, "changed")); !os.IsNotExist(err) {
				t.Fatalf("budgeted call wrote: %v", err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.cost != 0.51 {
				t.Fatalf("turn reset erased cumulative cost: %g", s.cost)
			}
		})
	}
}

func TestTurnBudgetUnavailableAndInvalidAccounting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens uint64
		usd    float64
		usage  *llm.Usage
	}{
		{"missing tokens", 10, 0, nil}, {"missing cost", 0, 1, nil},
		{"partial tokens", 10, 0, &llm.Usage{PromptTokens: 1, HasPromptTokens: true}},
		{"negative cost", 0, 1, &llm.Usage{Cost: -1, HasCost: true}},
		{"nonfinite cost", 0, 1, &llm.Usage{Cost: math.NaN(), HasCost: true}},
		{"overflow tokens", math.MaxUint64, 0, &llm.Usage{PromptTokens: math.MaxUint64, CompletionTokens: 1, HasPromptTokens: true, HasCompletionTokens: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, out := budgetSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
				return &llm.Completion{Usage: tc.usage, ToolCalls: []llm.ToolCall{{ID: "write", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}}}}, nil
			}), tc.tokens, tc.usd)
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("work")})
			assertBudgetStop(t, out.untilIdle(t), "accounting unavailable")
			if out.permissions.Load() != 0 || math.IsNaN(s.cost) || math.IsInf(s.cost, 0) || s.cost < 0 {
				t.Fatal("invalid accounting escaped into approvals or cumulative cost")
			}
		})
	}
}

func TestTurnBudgetSummaryCrossingNeverReplacesHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		manual bool
		tokens uint64
		usd    float64
		reason string
	}{{"automatic tokens", false, 10, 0, "token budget reached"}, {"manual tokens", true, 10, 0, "token budget reached"}, {"automatic USD", false, 0, 0.25, "USD budget reached"}, {"manual USD", true, 0, 0.25, "USD budget reached"}} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan llm.Request, 1)
			s, out := budgetSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				return &llm.Completion{Content: "Summary must not commit.", FinishReason: llm.FinishStop, Usage: budgetUsage(10, 0.25)}, nil
			}), tc.tokens, tc.usd)
			s.catalog = newCatalog([]llm.Model{{ID: "m", ContextLength: 16000}})
			old := strings.Repeat("old answer ", 1000)
			s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "old prompt"}, llm.Message{Role: llm.RoleAssistant, Content: old})
			prompt := "continue"
			if tc.manual {
				prompt = "/compact"
			}
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text(prompt)})
			updates := out.untilIdle(t)
			assertBudgetStop(t, updates, tc.reason)
			if len(requests) != 1 || len((<-requests).Tools) != 0 || s.cost != 0.25 {
				t.Fatal("summary usage was not charged exactly once")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.history) != 4 || s.history[2].Content != old {
				t.Fatal("over-budget summary replaced history")
			}
			for _, u := range updates {
				if c, ok := u.(acp.CompactionUpdate); ok && c.Status != acp.CompactionInProgress && c.Status != acp.CompactionFailed {
					t.Fatalf("budgeted compaction committed: %+v", c)
				}
			}
		})
	}
}

func TestTurnBudgetCancellationWinsOverCrossingResponse(t *testing.T) {
	var s *Session
	var out *budgetClient
	s, out = budgetSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		s.Cancel()
		return &llm.Completion{Usage: budgetUsage(10, 0.5), ToolCalls: []llm.ToolCall{{ID: "cancelled", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}}}}, nil
	}), 10, 0.5)
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("cancel at response")})
	updates := out.untilIdle(t)
	if idle := updates[len(updates)-1].(acp.StateUpdate); idle.StopReason != acp.StopCancelled || idle.Error != nil || out.permissions.Load() != 0 {
		t.Fatalf("budget overrode user cancellation: %+v", idle)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	assertToolPairs(t, s.history)
	if s.cost != 0.5 || !strings.Contains(s.history[len(s.history)-1].Content, "Cancelled") {
		t.Fatal("cancelled response lost accepted usage or its tool result")
	}
}

func TestTurnBudgetCostOverflowKeepsReportingFinite(t *testing.T) {
	requests := make(chan struct{}, 2)
	s, out := budgetSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		requests <- struct{}{}
		c := &llm.Completion{Usage: budgetUsage(0, 1e308), FinishReason: llm.FinishStop}
		if len(requests) == 1 {
			c.ToolCalls = []llm.ToolCall{{ID: "inspect", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}}}
		}
		return c, nil
	}), 0, math.MaxFloat64)
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("overflow")})
	updates := out.untilIdle(t)
	assertBudgetStop(t, updates, "overflow")
	if s.cost != 1e308 || len(requests) != 2 {
		t.Fatal("overflow produced nonfinite cumulative accounting")
	}
	for _, u := range updates {
		if usage, ok := u.(acp.UsageUpdate); ok && math.IsInf(usage.Cost.Amount, 0) {
			t.Fatal("nonfinite cost escaped to ACP")
		}
	}
}

func TestTurnBudgetSharesOrdinaryAndRollingSummaryAccounting(t *testing.T) {
	for _, rolling := range []bool{false, true} {
		t.Run(map[bool]string{false: "summary then answer", true: "rolling summaries"}[rolling], func(t *testing.T) {
			requests := make(chan llm.Request, 2)
			s, out := budgetSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				if len(requests) == 1 {
					return &llm.Completion{Content: "Earlier work.", FinishReason: llm.FinishStop, Usage: budgetUsage(3, 0.25)}, nil
				}
				c := &llm.Completion{Content: "Next output.", FinishReason: llm.FinishStop, Usage: budgetUsage(4, 0.25)}
				if !rolling {
					c.ToolCalls = []llm.ToolCall{{ID: "blocked", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"changed","content":"bad"}`}}}
				}
				return c, nil
			}), 7, 0.5)
			s.catalog = newCatalog([]llm.Model{{ID: "m", ContextLength: 16000}})
			n := 1
			if rolling {
				n = 3
			}
			for range n {
				s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "old"}, llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("old answer ", 700)})
			}
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("continue")})
			updates := out.untilIdle(t)
			assertBudgetStop(t, updates, "budget reached")
			if len(requests) != 2 || s.cost != 0.5 || out.permissions.Load() != 0 {
				t.Fatal("summary/answer allowances diverged or usage was charged twice")
			}
			first, last := <-requests, <-requests
			if len(first.Tools) != 0 || (len(last.Tools) == 0) != rolling {
				t.Fatal("fixture did not exercise mixed/rolling request paths")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			assertToolPairs(t, s.history)
			if rolling && len(s.history) != 8 {
				t.Fatal("unfinished rolling compaction replaced history")
			}
		})
	}
}

func TestTurnBudgetChargesInvalidOrAbandonedSummary(t *testing.T) {
	for _, content := range []string{"", strings.Repeat("s", 16000)} {
		t.Run(map[bool]string{false: "invalid", true: "abandoned"}[content != ""], func(t *testing.T) {
			seen := make(chan *turnBudget, 1)
			var s *Session
			var out *budgetClient
			s, out = budgetSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
				seen <- s.budget
				return &llm.Completion{Content: content, FinishReason: llm.FinishStop, Usage: budgetUsage(3, 0.25)}, nil
			}), 10, 1)
			s.catalog = newCatalog([]llm.Model{{ID: "m", ContextLength: 16000}})
			s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "old"}, llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("answer ", 500)})
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("/compact")})
			updates := out.untilIdle(t)
			idle := updates[len(updates)-1].(acp.StateUpdate)
			b := <-seen
			if idle.StopReason != acp.StopError || b.tokens != 3 || b.cost != 0.25 || s.cost != 0.25 {
				t.Fatalf("abandoned usage was lost: idle=%+v tokens=%d USD=%g cumulative=%g", idle, b.tokens, b.cost, s.cost)
			}
		})
	}
}
