package mcp

import (
	"bufio"
	"context"
	"encoding/json"
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
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		_ = json.Unmarshal(sc.Bytes(), &req)
		if req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]string{"name": "fake", "version": "1"}}
		case "tools/list":
			var p struct{ Cursor string }
			_ = json.Unmarshal(req.Params, &p)
			name, next := "read_file", "second"
			if p.Cursor != "" {
				name, next = "schedule_task", ""
			}
			result = map[string]any{"tools": []any{map[string]any{"name": name, "description": "fixture", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": next}
		case "tools/call":
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
		_ = w.Flush()
	}
	os.Exit(0)
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
	m := New(context.Background(), t.TempDir())
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
