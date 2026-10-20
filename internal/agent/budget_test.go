package agent

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/llm"
)

func budgetUsage(tokens uint64, cost float64) *llm.Usage {
	return &llm.Usage{PromptTokens: tokens, Cost: cost, HasPromptTokens: true, HasCompletionTokens: true, HasCost: true}
}

func TestTurnBudgetThresholdsAndCancellationPriority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget turnBudget
		first  *llm.Usage
		last   *llm.Usage
	}{
		{"tokens exact", turnBudget{maxTokens: 10}, budgetUsage(4, 0), budgetUsage(6, 0)},
		{"tokens above", turnBudget{maxTokens: 10}, budgetUsage(4, 0), budgetUsage(7, 0)},
		{"USD exact", turnBudget{maxCost: 1}, budgetUsage(0, 0.25), budgetUsage(0, 0.75)},
		{"USD above", turnBudget{maxCost: 1}, budgetUsage(0, 0.25), budgetUsage(0, 1)},
		{"joint tokens first", turnBudget{maxTokens: 10, maxCost: 1}, budgetUsage(4, 0.25), budgetUsage(6, 0.25)},
		{"joint USD first", turnBudget{maxTokens: 10, maxCost: 1}, budgetUsage(4, 0.25), budgetUsage(1, 0.75)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.budget
			if err := b.check(context.Background()); err != nil {
				t.Fatal("preflight required usage before the first response")
			}
			if err := b.record(tc.first); err != nil {
				t.Fatalf("stopped below threshold: %v", err)
			}
			if err := b.record(tc.last); err == nil || !strings.Contains(err.Error(), "budget reached") {
				t.Fatalf("threshold did not stop work: %v", err)
			}
			if err := b.record(budgetUsage(0, 0)); err == nil {
				t.Fatal("budget failure was not sticky")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := b.check(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("budget overrode cancellation: %v", err)
			}
		})
	}
}

func TestTurnBudgetAccountingAvailabilityAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		budget  turnBudget
		usage   *llm.Usage
		wantErr bool
	}{
		{"disabled metrics", turnBudget{}, nil, false},
		{"zero/free", turnBudget{maxTokens: 1, maxCost: 1}, budgetUsage(0, 0), false},
		{"absent tokens", turnBudget{maxTokens: 10}, nil, true},
		{"absent cost", turnBudget{maxCost: 1}, nil, true},
		{"missing output", turnBudget{maxTokens: 10}, &llm.Usage{PromptTokens: 1, HasPromptTokens: true}, true},
		{"token only", turnBudget{maxTokens: 10}, &llm.Usage{HasPromptTokens: true, HasCompletionTokens: true}, false},
		{"cost only", turnBudget{maxCost: 1}, &llm.Usage{Cost: 0, HasCost: true}, false},
		{"negative cost", turnBudget{maxCost: 1}, &llm.Usage{Cost: -1, HasCost: true}, true},
		{"NaN cost", turnBudget{maxCost: 1}, &llm.Usage{Cost: math.NaN(), HasCost: true}, true},
		{"infinite cost", turnBudget{maxCost: 1}, &llm.Usage{Cost: math.Inf(1), HasCost: true}, true},
		{"token sum overflow", turnBudget{maxTokens: math.MaxUint64}, &llm.Usage{PromptTokens: math.MaxUint64, CompletionTokens: 1, HasPromptTokens: true, HasCompletionTokens: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.budget.record(tc.usage)
			if (err != nil) != tc.wantErr || err != nil && !strings.Contains(err.Error(), "accounting unavailable") {
				t.Fatalf("accounting: err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
	b := turnBudget{maxTokens: math.MaxUint64, maxCost: math.MaxFloat64}
	if err := b.record(budgetUsage(math.MaxUint64-1, 1e308)); err != nil {
		t.Fatal(err)
	}
	if err := b.record(budgetUsage(2, 1e308)); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("counters wrapped: %v", err)
	}
}
