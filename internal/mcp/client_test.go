package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCPHelper is a separate isolated stdio server, not a mocked transport.
func TestMCPHelper(t *testing.T) {
	if os.Getenv("ACPLAUSE_MCP_HELPER") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	changed := false
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		_ = json.Unmarshal(sc.Bytes(), &req)
		if path := os.Getenv("ACPLAUSE_MCP_TRACE"); path != "" {
			f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if f != nil {
				fmt.Fprintln(f, string(sc.Bytes()))
				f.Close()
			}
		}
		if req.ID == nil {
			continue
		}
		var result any
		variant := os.Getenv("ACPLAUSE_MCP_CASE")
		switch req.Method {
		case "initialize":
			version := "2025-11-25"
			if variant == "version" {
				version = "unsupported"
			}
			result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]string{"name": "fake", "version": "1"}}
		case "tools/list":
			var p struct{ Cursor string }
			_ = json.Unmarshal(req.Params, &p)
			name, next := "read_file", "second"
			if p.Cursor != "" {
				name, next = "schedule_task", ""
				if variant == "duplicate" {
					name = "read_file"
				}
				if variant == "cursor" {
					next = "second"
				}
			}
			result = map[string]any{"tools": []any{map[string]any{"name": name, "description": fmt.Sprint("fixture-", changed), "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": next}
		case "tools/call":
			if strings.Contains(string(req.Params), `"wait":true`) {
				continue
			}
			var p struct {
				Name      string
				Arguments json.RawMessage
			}
			_ = json.Unmarshal(req.Params, &p)
			result = map[string]any{"content": []any{}, "structuredContent": p.Arguments}
		case "ping":
			result = map[string]any{}
		default:
			continue
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		fmt.Fprintln(w, string(b))
		if req.Method == "tools/call" && os.Getenv("ACPLAUSE_MCP_REFRESH") == "1" {
			changed = true
			fmt.Fprintln(w, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
		}
		_ = w.Flush()
		if req.Method == "initialize" && variant == "control" {
			fmt.Fprintln(w, `{"jsonrpc":"2.0","id":"server-ping","method":"ping"}`)
			_ = w.Flush()
		}
	}
	if os.Getenv("ACPLAUSE_MCP_CASE") == "stubborn" {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestMCPFailedCommitDefersProcessCleanup(t *testing.T) {
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := helperConfig(t, "stubborn")
	cfg.Env["ACPLAUSE_MCP_CASE"] = "stubborn"
	change, err := m.Prepare(ctx, []Config{cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer change.Discard()
	cancel()
	// Session publication holds its lock. Even a rejected publication must
	// return without waiting for EOF/TERM; its caller discards after unlocking.
	done := make(chan error, 1)
	go func() { done <- change.Commit() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled publication succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("publication blocked on process cleanup")
	}
}

func TestMCPRejectsInvalidDiscovery(t *testing.T) {
	for _, variant := range []string{"version", "duplicate", "cursor"} {
		t.Run(variant, func(t *testing.T) {
			m := New(context.Background(), t.TempDir(), nil)
			defer m.Close()
			cfg := helperConfig(t, "invalid")
			cfg.Env["ACPLAUSE_MCP_CASE"] = variant
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := m.Prepare(ctx, []Config{cfg}); err == nil {
				t.Fatal("invalid discovery accepted")
			}
			if len(m.Snapshot()) != 0 {
				t.Fatal("failed discovery published a partial catalog")
			}
		})
	}
}

func TestMCPRejectsMalformedResponses(t *testing.T) {
	for _, raw := range []string{`[]`, `{"jsonrpc":"1.0","id":"a","result":{}}`, `{"jsonrpc":"2.0","id":null,"result":{}}`, `{"jsonrpc":"2.0","id":"a","result":{},"error":{"code":1,"message":"bad"}}`, strings.Repeat("x", maxMessage+1)} {
		p := newPeer(context.Background())
		p.receive([]byte(raw))
		if p.err() == nil {
			p.cancel()
			t.Fatal("invalid response did not retire connection")
		}
		p.cancel()
	}
}

func helperConfig(t *testing.T, name string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Type: "stdio", Name: name, Command: exe, Args: []string{"-test.run=^TestMCPHelper$"}, Env: map[string]string{"ACPLAUSE_MCP_HELPER": "1"}}
}

func TestMCPStdioDiscoveryAndRawArguments(t *testing.T) {
	m := New(context.Background(), t.TempDir(), nil)
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	change, err := m.Prepare(ctx, []Config{helperConfig(t, "a.b"), helperConfig(t, "a-b")})
	if err != nil {
		t.Fatal(err)
	}
	if err := change.Commit(); err != nil {
		t.Fatal(err)
	}
	tools := m.Snapshot()
	if len(tools) != 4 {
		t.Fatalf("tools: %+v", tools)
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name] || len(tool.Name) > 64 || tool.Name == "read_file" {
			t.Fatal("alias collision", tool.Name)
		}
		seen[tool.Name] = true
	}
	args := `{"schedule":{"type":"interval","everyMs":3600000},"id":9007199254740993,"array":[{"taskId":"opaque"}]}`
	result, err := tools[0].Call(ctx, args)
	if err != nil || string(result.StructuredContent) != args {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := tools[0].Call(ctx, `"not an object"`); err == nil {
		t.Fatal("accepted string arguments")
	}
	old := tools[0]
	change, err = m.Prepare(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := change.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Call(ctx, `{}`); err == nil {
		t.Fatal("revoked tool ran")
	}
}

func TestMCPEnvironment(t *testing.T) {
	t.Setenv("ACPLAUSE_API_KEY", "must-not-leak")
	env := processEnvironment(map[string]string{"EXPLICIT": "value"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "must-not-leak") || !strings.Contains(joined, "EXPLICIT=value") {
		t.Fatal("unsafe environment")
	}
	if !filepath.IsAbs(helperConfig(t, "s").Command) {
		t.Fatal("helper command is not absolute")
	}
}

func TestMCPCatalogRefreshRevokesOldDefinition(t *testing.T) {
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := helperConfig(t, "s")
	cfg.Env["ACPLAUSE_MCP_REFRESH"] = "1"
	change, err := m.Prepare(ctx, []Config{cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err = change.Commit(); err != nil {
		t.Fatal(err)
	}
	old := m.Snapshot()[0]
	if _, err = old.Call(ctx, `{}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Context().Done():
	case <-ctx.Done():
		t.Fatal("list changed did not refresh catalog")
	}
	if _, err = old.Call(ctx, `{}`); err == nil {
		t.Fatal("old definition was not revoked")
	}
	tools := m.Snapshot()
	if len(tools) != 2 || tools[0].Description != "fixture-true" || tools[0].Name != old.Name {
		t.Fatalf("refresh: %+v", tools)
	}
}

// scriptedTransport models bytes delivered just before a transport terminates.
type scriptedTransport struct {
	send func(context.Context, []byte) error
}

func (t scriptedTransport) Send(ctx context.Context, b []byte) error { return t.send(ctx, b) }
func (scriptedTransport) Close() error                               { return nil }
func (scriptedTransport) Initialized(string)                         {}

func TestMCPDeliveredResponseWinsDisconnect(t *testing.T) {
	for range 100 {
		p := newPeer(context.Background())
		p.t = scriptedTransport{send: func(_ context.Context, b []byte) error {
			var req wire
			_ = json.Unmarshal(b, &req)
			p.receive([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, req.ID)))
			p.fail(errors.New("disconnected"))
			return nil
		}}
		var result struct{ OK bool }
		err := p.call(context.Background(), "tools/call", map[string]any{}, &result)
		p.cancel()
		if err != nil || !result.OK {
			t.Fatalf("delivered result lost: %+v, %v", result, err)
		}
	}
}

func TestMCPCancelDuringSendNotifiesServer(t *testing.T) {
	for range 50 {
		p := newPeer(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		cancelled := make(chan struct{})
		p.t = scriptedTransport{send: func(_ context.Context, b []byte) error {
			var msg wire
			_ = json.Unmarshal(b, &msg)
			if msg.Method == "notifications/cancelled" {
				close(cancelled)
				return nil
			}
			cancel() // the request was written, then the caller left
			return ctx.Err()
		}}
		if err := p.call(ctx, "tools/call", map[string]any{}, nil); err == nil {
			t.Fatal("cancelled call succeeded")
		}
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Fatal("server was not told the call was cancelled")
		}
		p.cancel()
	}
}

func TestMCPStdioDeliversPongAndCancellation(t *testing.T) {
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := helperConfig(t, "control")
	cfg.Env["ACPLAUSE_MCP_CASE"] = "control"
	trace := filepath.Join(t.TempDir(), "trace")
	cfg.Env["ACPLAUSE_MCP_TRACE"] = trace
	change, err := m.Prepare(ctx, []Config{cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err = change.Commit(); err != nil {
		t.Fatal(err)
	}
	callCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { _, err := m.Snapshot()[0].Call(callCtx, `{"wait":true}`); done <- err }()
	waitTrace := func(want string) {
		t.Helper()
		for {
			b, _ := os.ReadFile(trace)
			if strings.Contains(string(b), want) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("server never received %s; trace=%s", want, b)
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitTrace(`"method":"tools/call"`)
	stop()
	if err := <-done; err == nil {
		t.Fatal("cancelled call succeeded")
	}
	waitTrace(`"method":"notifications/cancelled"`)
	waitTrace(`"id":"server-ping","result":{}`)
}

func TestMCPStdioSendWaitsForWrite(t *testing.T) {
	p := newPeer(context.Background())
	tr, err := startStdio(p, helperConfig(t, "send"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.t = tr
	defer tr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tr.Send(ctx, []byte(`{"jsonrpc":"2.0","method":"test"}`)) }()
	for len(tr.writes) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("not queued")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-done:
		t.Fatalf("Send returned before writer started: %v", err)
	default:
	}
	tr.start()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
