package llm

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestUsagePresenceAndCheckedAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, raw        string
		prompt, output   bool
		cost             bool
		tokens           uint64
		usd              float64
		tokensOK, costOK bool
	}{
		{name: "omitted", raw: `{}`},
		{name: "availability cannot be supplied by provider flags", raw: `{"HasPromptTokens":true,"HasCompletionTokens":true,"HasCost":true}`},
		{name: "null fields", raw: `{"prompt_tokens":null,"completion_tokens":null,"cost":null}`},
		{name: "zero tokens without cost", raw: `{"prompt_tokens":0,"completion_tokens":0}`, prompt: true, output: true, tokensOK: true},
		{name: "free response", raw: `{"prompt_tokens":10,"completion_tokens":5,"cost":0}`, prompt: true, output: true, cost: true, tokens: 15, tokensOK: true, costOK: true},
		{name: "all zeros", raw: `{"prompt_tokens":0,"completion_tokens":0,"cost":0}`, prompt: true, output: true, cost: true, tokensOK: true, costOK: true},
		{name: "missing input", raw: `{"completion_tokens":2,"cost":0.25}`, output: true, cost: true, usd: 0.25, costOK: true},
		{name: "missing output", raw: `{"prompt_tokens":2,"total_tokens":2}`, prompt: true},
		{name: "null input", raw: `{"prompt_tokens":null,"completion_tokens":0,"cost":0}`, output: true, cost: true, costOK: true},
		{name: "null output", raw: `{"prompt_tokens":0,"completion_tokens":null}`, prompt: true},
		{name: "null cost", raw: `{"prompt_tokens":1,"completion_tokens":2,"cost":null}`, prompt: true, output: true, tokens: 3, tokensOK: true},
		{name: "subsets are not extra tokens", raw: `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":999,"cost":0.001,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2},"future":{"data":true}}`, prompt: true, output: true, cost: true, tokens: 15, usd: 0.001, tokensOK: true, costOK: true},
		{name: "negative cost is present but invalid", raw: `{"prompt_tokens":1,"completion_tokens":2,"cost":-0.01}`, prompt: true, output: true, cost: true, tokens: 3, tokensOK: true},
		{name: "maximum token sum", raw: `{"prompt_tokens":18446744073709551615,"completion_tokens":0}`, prompt: true, output: true, tokens: math.MaxUint64, tokensOK: true},
		{name: "token sum overflow", raw: `{"prompt_tokens":18446744073709551615,"completion_tokens":1}`, prompt: true, output: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u Usage
			if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
				t.Fatal(err)
			}
			if u.HasPromptTokens != tc.prompt || u.HasCompletionTokens != tc.output || u.HasCost != tc.cost {
				t.Fatalf("presence: %+v", u)
			}
			if tokens, ok := u.ReportedTokens(); tokens != tc.tokens || ok != tc.tokensOK {
				t.Fatalf("tokens = %d, %v; want %d, %v", tokens, ok, tc.tokens, tc.tokensOK)
			}
			if usd, ok := u.ReportedCost(); usd != tc.usd || ok != tc.costOK {
				t.Fatalf("cost = %g, %v; want %g, %v", usd, ok, tc.usd, tc.costOK)
			}
		})
	}
}

func TestUsageAvailabilityResetAndCompatibility(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.25,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 || u.Cost != 0.25 || u.PromptDetails.CachedTokens != 4 || u.CompletionDetails.ReasoningTokens != 2 {
		t.Fatalf("legacy fields changed: %+v", u)
	}
	encoded, err := json.Marshal(u)
	if err != nil || strings.Contains(string(encoded), "Has") {
		t.Fatalf("availability leaked into JSON: %s, %v", encoded, err)
	}
	var roundTrip Usage
	if err := json.Unmarshal(encoded, &roundTrip); err != nil || !reflect.DeepEqual(u, roundTrip) {
		t.Fatalf("valid usage round trip changed: %+v, %v", roundTrip, err)
	}
	for _, raw := range []string{`{}`, `null`, `{"cost":null}`} {
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		if u != (Usage{}) {
			t.Fatalf("decode retained stale values: %+v", u)
		}
	}
	// Legacy consumers can still construct and read numeric fields. A new
	// enforcement consumer must require explicit availability for such values.
	u = Usage{PromptTokens: 3, CompletionTokens: 4, Cost: 0.25}
	if _, ok := u.ReportedTokens(); ok {
		t.Fatal("inferred presence from nonzero numeric fields")
	}
	if _, ok := u.ReportedCost(); ok {
		t.Fatal("inferred cost presence from its value")
	}
}

func TestUsageRejectsMalformedAccounting(t *testing.T) {
	for _, raw := range []string{
		`[]`, `42`, `"usage"`, `{"prompt_tokens":-1}`, `{"completion_tokens":-1}`,
		`{"prompt_tokens":1.5}`, `{"prompt_tokens":18446744073709551616}`,
		`{"completion_tokens":"secret-marker"}`, `{"cost":"secret-marker"}`,
		`{"cost":true}`, `{"cost":NaN}`, `{"cost":1e309}`, `{"cost":{}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			u := Usage{PromptTokens: 7}
			if err := json.Unmarshal([]byte(raw), &u); err == nil || strings.Contains(err.Error(), "secret-marker") {
				t.Fatalf("malformed/unsafe accounting error: %v", err)
			}
			if u != (Usage{PromptTokens: 7}) {
				t.Fatal("failed decode published partial usage")
			}
		})
	}
	for _, cost := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		u := Usage{Cost: cost, HasCost: true}
		if _, ok := u.ReportedCost(); ok {
			t.Fatalf("invalid cost accepted: %g", cost)
		}
	}
	var missing *Usage
	if _, ok := missing.ReportedTokens(); ok {
		t.Fatal("nil usage supplied tokens")
	}
	if _, ok := missing.ReportedCost(); ok {
		t.Fatal("nil usage supplied cost")
	}
}

func TestUsageAvailabilityThroughStream(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		present     bool
	}{
		{"absent", "", false},
		{"null", `,"usage":null`, false},
		{"empty", `,"usage":{}`, true},
		{"null cost", `,"usage":{"prompt_tokens":0,"completion_tokens":0,"cost":null}`, true},
		{"free", `,"usage":{"prompt_tokens":0,"completion_tokens":0,"cost":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := data(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]`+tc.usage+`}`) + data(`[DONE]`)
			c, err := readStream(context.Background(), strings.NewReader(wire), func(Delta) {})
			if err != nil || c.Content != "ok" || (c.Usage != nil) != tc.present {
				t.Fatalf("stream accounting: %+v, %v", c, err)
			}
			if _, ok := c.Usage.ReportedCost(); ok != (tc.name == "free") {
				t.Fatalf("stream lost cost presence: %+v", c.Usage)
			}
			if tokens, ok := c.Usage.ReportedTokens(); tokens != 0 || ok != (tc.name == "free" || tc.name == "null cost") {
				t.Fatalf("stream tokens = %d, %v", tokens, ok)
			}
		})
	}
}

func TestUsageMalformedStreamKeepsProtocolErrorsSanitized(t *testing.T) {
	for _, raw := range []string{`[]`, `{"prompt_tokens":-1}`, `{"completion_tokens":"secret-marker"}`, `{"cost":"secret-marker"}`} {
		wire := data(`{"choices":[],"usage":`+raw+`,"future":"secret-marker"}`) + data(`[DONE]`)
		c, err := readStream(context.Background(), strings.NewReader(wire), func(Delta) {})
		failure, ok := errors.AsType[*Error](err)
		if c != nil || !ok || failure.Kind != KindProtocol || strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("malformed usage exposed a raw chunk or changed error class: %+v, %v", c, err)
		}
	}
}
