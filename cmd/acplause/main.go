// Command acplause is an ACP v2 agent that serves an editor over stdio and
// thinks with any OpenAI-compatible Chat Completions API, OpenRouter first.
//
// Configuration comes from the environment:
//
//	ACPLAUSE_API_KEY        API key; falls back to OPENROUTER_API_KEY
//	ACPLAUSE_BASE_URL       API root; default https://openrouter.ai/api/v1
//	ACPLAUSE_READ_CONCURRENCY built-in read workers; default 1, range 1..8
//	ACPLAUSE_MAX_ROUNDS     model requests per turn; default 200, 0 disables
//	ACPLAUSE_BASH_TIMEOUT   bash tool timeout; default 30m, 0 disables
//	ACPLAUSE_MCP_TIMEOUT    MCP tool timeout; default 1h, 0 disables
//	ACPLAUSE_TURN_TIMEOUT   accepted turn timeout; default 0 (unlimited)
//	ACPLAUSE_MAX_TURN_TOKENS reported input+output tokens per turn; default 0
//	ACPLAUSE_MAX_TURN_COST_USD reported USD per turn; default 0
//	ACPLAUSE_LOG_LEVEL      debug, info, warn or error; default info
//	ACPLAUSE_DATA_DIR       session store directory; default ~/.acplause
//
// stdout carries only protocol messages; logs go to stderr.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/agent"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
	"github.com/anesthetised/acplause/internal/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "acplause:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := llm.New(llm.Config{
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
		Header: http.Header{
			"HTTP-Referer":       {"https://github.com/anesthetised/acplause"},
			"X-OpenRouter-Title": {"acplause"},
		},
	})
	dir := cfg.DataDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("finding the data directory: %w; set ACPLAUSE_DATA_DIR", err)
		}
		dir = filepath.Join(home, ".acplause")
	}
	st, err := store.Open(dir, processID())
	if err != nil {
		return fmt.Errorf("opening the session store: %w", err)
	}
	defer st.Close()

	conn := jsonrpc.NewConn(os.Stdin, os.Stdout, log)
	ag := agent.New(agent.Config{
		Info:            acp.Implementation{Name: "acplause", Version: version()},
		ReadConcurrency: cfg.ReadConcurrency,
		MaxRounds:       &cfg.MaxRounds,
		MCPTimeout:      &cfg.MCPTimeout,
		TurnTimeout:     &cfg.TurnTimeout,
		MaxTurnTokens:   cfg.MaxTurnTokens,
		MaxTurnCostUSD:  cfg.MaxTurnCostUSD,
		Tools:           tools.Options{BashTimeout: &cfg.BashTimeout},
		Log:             log,
		Store:           st,
	}, conn, client, client.Models)

	log.Info("serving", "base_url", cfg.BaseURL, "data_dir", dir)
	err = conn.Serve(ctx, ag.Methods())
	ag.Shutdown()
	_ = conn.Close()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// processID tells this process's session leases from other processes'.
func processID() string {
	return fmt.Sprintf("%d-%s", os.Getpid(), rand.Text())
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
