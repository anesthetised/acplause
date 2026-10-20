package main

import (
	"math"
	"strings"
	"testing"
)

func TestTurnBudgetConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  uint64
		valid bool
	}{
		{"", 0, true}, {"0", 0, true}, {"15", 15, true}, {"18446744073709551615", math.MaxUint64, true},
		{"18446744073709551616", 0, false}, {"-1", 0, false}, {"1.5", 0, false}, {"secret-marker", 0, false},
	} {
		cfg, err := loadConfig(env(map[string]string{"ACPLAUSE_MAX_TURN_TOKENS": tc.value}))
		if (err == nil) != tc.valid || tc.valid && cfg.MaxTurnTokens != tc.want {
			t.Errorf("token config: got=%d err=%v valid=%v", cfg.MaxTurnTokens, err, tc.valid)
		}
		if err != nil && (!strings.Contains(err.Error(), "ACPLAUSE_MAX_TURN_TOKENS") || strings.Contains(err.Error(), "secret-marker")) {
			t.Fatalf("unsafe token configuration error: %v", err)
		}
	}
	for _, tc := range []struct {
		value string
		want  float64
		valid bool
	}{
		{"", 0, true}, {"0", 0, true}, {"0.0", 0, true}, {"0e-999", 0, true}, {"0.25", 0.25, true},
		{"1e-3", 0.001, true}, {"1.7976931348623157e308", math.MaxFloat64, true},
		{"-0.1", 0, false}, {"NaN", 0, false}, {"Inf", 0, false}, {"1e309", 0, false},
		{"1e-400", 0, false}, {"0x1p2", 0, false}, {"1_000", 0, false}, {"secret-marker", 0, false},
	} {
		cfg, err := loadConfig(env(map[string]string{"ACPLAUSE_MAX_TURN_COST_USD": tc.value}))
		if (err == nil) != tc.valid || tc.valid && cfg.MaxTurnCostUSD != tc.want {
			t.Errorf("USD config: got=%g err=%v valid=%v", cfg.MaxTurnCostUSD, err, tc.valid)
		}
		if err != nil && (!strings.Contains(err.Error(), "ACPLAUSE_MAX_TURN_COST_USD") || strings.Contains(err.Error(), "secret-marker")) {
			t.Fatalf("unsafe USD configuration error: %v", err)
		}
	}
}
