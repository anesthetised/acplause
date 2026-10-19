package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/llm"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"OPENROUTER_API_KEY":    "sk-or",
		"ACPLAUSE_MAX_ROUNDS":   "7",
		"ACPLAUSE_BASH_TIMEOUT": "90s",
		"ACPLAUSE_LOG_LEVEL":    "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config{BaseURL: llm.DefaultBaseURL, APIKey: "sk-or", MaxRounds: 7, BashTimeout: 90 * time.Second, MCPTimeout: 10 * time.Minute, LogLevel: slog.LevelDebug}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}

	cfg, _ = loadConfig(env(map[string]string{"ACPLAUSE_API_KEY": "own", "OPENROUTER_API_KEY": "or"}))
	if cfg.APIKey != "own" {
		t.Fatalf("ACPLAUSE_API_KEY must win, got %q", cfg.APIKey)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	_, err := loadConfig(env(map[string]string{
		"ACPLAUSE_MAX_ROUNDS":   "0",
		"ACPLAUSE_BASH_TIMEOUT": "soon",
		"ACPLAUSE_LOG_LEVEL":    "loud",
	}))
	for _, want := range []string{"ACPLAUSE_MAX_ROUNDS", "ACPLAUSE_BASH_TIMEOUT", "ACPLAUSE_LOG_LEVEL"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error about %s, got %v", want, err)
		}
	}
}

func TestLoadConfigWithoutModel(t *testing.T) {
	if _, err := loadConfig(env(nil)); err != nil {
		t.Fatalf("model selection belongs to the ACP client: %v", err)
	}
}

func TestMCPTimeout(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  time.Duration
		valid bool
	}{{"", 10 * time.Minute, true}, {"0", 0, true}, {"30m", 30 * time.Minute, true}, {"-1s", 0, false}, {"later", 0, false}} {
		cfg, err := loadConfig(env(map[string]string{"ACPLAUSE_MCP_TIMEOUT": tt.value}))
		if (err == nil) != tt.valid || tt.valid && cfg.MCPTimeout != tt.want {
			t.Errorf("%q: timeout=%v err=%v", tt.value, cfg.MCPTimeout, err)
		}
	}
}
