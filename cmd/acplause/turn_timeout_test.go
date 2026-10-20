package main

import (
	"strings"
	"testing"
	"time"
)

func TestTurnTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"", 0, true}, {"0", 0, true}, {"0s", 0, true},
		{"15m", 15 * time.Minute, true}, {"1.5s", 1500 * time.Millisecond, true},
		{"-1ns", 0, false}, {"later", 0, false}, {"999999999999h", 0, false},
		{"secret-marker", 0, false},
	} {
		cfg, err := loadConfig(env(map[string]string{"ACPLAUSE_TURN_TIMEOUT": tc.value}))
		if (err == nil) != tc.valid || tc.valid && cfg.TurnTimeout != tc.want {
			t.Errorf("valid=%v timeout=%v err=%v", tc.valid, cfg.TurnTimeout, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "ACPLAUSE_TURN_TIMEOUT") || strings.Contains(err.Error(), "secret-marker")) {
			t.Fatalf("unsafe or unclear configuration error: %v", err)
		}
	}
}
