package main

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/anesthetised/acplause/internal/agent"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/tools"
)

// config is read from the environment.
type config struct {
	BaseURL     string
	APIKey      string
	MaxRounds   int
	BashTimeout time.Duration
	MCPTimeout  time.Duration
	LogLevel    slog.Level
	DataDir     string // "" selects ~/.acplause
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		BaseURL:     cmp.Or(getenv("ACPLAUSE_BASE_URL"), llm.DefaultBaseURL),
		APIKey:      cmp.Or(getenv("ACPLAUSE_API_KEY"), getenv("OPENROUTER_API_KEY")),
		DataDir:     getenv("ACPLAUSE_DATA_DIR"),
		MaxRounds:   agent.DefaultMaxRounds,
		BashTimeout: tools.DefaultBashTimeout,
		MCPTimeout:  agent.DefaultMCPTimeout,
	}

	var errs []error
	if v := getenv("ACPLAUSE_MCP_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("ACPLAUSE_MCP_TIMEOUT: want a nonnegative duration, got %q", v))
		} else {
			cfg.MCPTimeout = d
		}
	}
	if v := getenv("ACPLAUSE_MAX_ROUNDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("ACPLAUSE_MAX_ROUNDS: want a nonnegative integer, got %q", v))
		}
		cfg.MaxRounds = n
	}
	if v := getenv("ACPLAUSE_BASH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("ACPLAUSE_BASH_TIMEOUT: want a nonnegative duration such as 90s, got %q", v))
		}
		cfg.BashTimeout = d
	}
	if v := getenv("ACPLAUSE_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("ACPLAUSE_LOG_LEVEL: %w", err))
		}
	}
	return cfg, errors.Join(errs...)
}
