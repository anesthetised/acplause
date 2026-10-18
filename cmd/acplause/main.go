// Command acplause is an ACP v2 agent that serves an editor over stdio and
// thinks with any OpenAI-compatible Chat Completions API, OpenRouter first.
//
// Configuration comes from the environment:
//
//	ACPLAUSE_API_KEY        API key; falls back to OPENROUTER_API_KEY
//	ACPLAUSE_BASE_URL       API root; default https://openrouter.ai/api/v1
//	ACPLAUSE_MAX_ROUNDS     model requests per turn; default 25
//	ACPLAUSE_BASH_TIMEOUT   bash tool timeout; default 2m
//	ACPLAUSE_LOG_LEVEL      debug, info, warn or error; default info
//
// stdout carries only protocol messages; logs go to stderr.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/agent"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
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
	conn := jsonrpc.NewConn(os.Stdin, os.Stdout, log)
	ag := agent.New(agent.Config{
		Info:      acp.Implementation{Name: "acplause", Version: version()},
		MaxRounds: cfg.MaxRounds,
		Tools:     tools.Options{BashTimeout: cfg.BashTimeout},
		Log:       log,
	}, conn, client, client.Models)

	log.Info("serving", "base_url", cfg.BaseURL)
	err = conn.Serve(ctx, ag.Methods())
	ag.Shutdown()
	_ = conn.Close()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
