package main

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/anesthetised/acplause/internal/agent"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/tools"
)

// config is read from the environment.
type config struct {
	BaseURL         string
	APIKey          string
	ReadConcurrency int
	MaxRounds       int
	BashTimeout     time.Duration
	MCPTimeout      time.Duration
	TurnTimeout     time.Duration
	MaxTurnTokens   uint64
	MaxTurnCostUSD  float64
	LogLevel        slog.Level
	DataDir         string // "" selects ~/.acplause
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		BaseURL:         cmp.Or(getenv("ACPLAUSE_BASE_URL"), llm.DefaultBaseURL),
		APIKey:          cmp.Or(getenv("ACPLAUSE_API_KEY"), getenv("OPENROUTER_API_KEY")),
		DataDir:         getenv("ACPLAUSE_DATA_DIR"),
		ReadConcurrency: 1,
		MaxRounds:       agent.DefaultMaxRounds,
		BashTimeout:     tools.DefaultBashTimeout,
		MCPTimeout:      agent.DefaultMCPTimeout,
	}

	var errs []error
	if v := getenv("ACPLAUSE_READ_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 8 {
			errs = append(errs, errors.New("ACPLAUSE_READ_CONCURRENCY: want an integer from 1 through 8"))
		} else {
			cfg.ReadConcurrency = n
		}
	}
	if v := getenv("ACPLAUSE_MAX_TURN_TOKENS"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			errs = append(errs, errors.New("ACPLAUSE_MAX_TURN_TOKENS: want a nonnegative base-10 integer fitting uint64"))
		} else {
			cfg.MaxTurnTokens = n
		}
	}
	if v := getenv("ACPLAUSE_MAX_TURN_COST_USD"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		mantissa, _, _ := strings.Cut(strings.ToLower(v), "e")
		underflow := n == 0 && strings.ContainsAny(mantissa, "123456789")
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || strings.ContainsAny(v, "xXpP_") || underflow {
			errs = append(errs, errors.New("ACPLAUSE_MAX_TURN_COST_USD: want a finite nonnegative representable decimal USD value"))
		} else {
			cfg.MaxTurnCostUSD = n
		}
	}
	if v := getenv("ACPLAUSE_TURN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, errors.New("ACPLAUSE_TURN_TIMEOUT: want a nonnegative duration such as 15m"))
		} else {
			cfg.TurnTimeout = d
		}
	}
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
