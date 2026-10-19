// Command mcp is an isolated MCP fixture used by the stdio e2e tests.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	if path := os.Getenv("FIXTURE_PID"); path != "" {
		_ = os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0600)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 8<<20)
	var waiting json.RawMessage
	for sc.Scan() {
		var req request
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.Method == "notifications/cancelled" {
			record(req.Method, req.Params)
			if waiting != nil {
				respond(waiting, map[string]any{"content": []any{map[string]string{"type": "text", "text": "LATE_RESULT_MUST_NOT_APPEAR"}}}, nil)
				waiting = nil
			}
			continue
		}
		if req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			if os.Getenv("FIXTURE_GATE") != "" {
				_ = os.WriteFile(os.Getenv("FIXTURE_GATE")+".entered", []byte("entered"), 0600)
				for {
					if _, err := os.Stat(os.Getenv("FIXTURE_GATE")); err == nil {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			respond(req.ID, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}, nil)
		case "tools/list":
			var p struct{ Cursor string }
			_ = json.Unmarshal(req.Params, &p)
			names, next := []string{"read_file", "schedule_task"}, "second"
			if p.Cursor != "" {
				names, next = []string{"delegate_task", "fail", "wait", "crash", "image"}, ""
			}
			var tools []any
			for _, name := range names {
				tools = append(tools, map[string]any{"name": name, "description": "fixture:" + name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"schedule": map[string]string{"type": "object"}}}, "annotations": map[string]bool{"readOnlyHint": true}})
			}
			respond(req.ID, map[string]any{"tools": tools, "nextCursor": next}, nil)
		case "tools/call":
			var p struct {
				Name      string
				Arguments json.RawMessage
			}
			_ = json.Unmarshal(req.Params, &p)
			record(p.Name, p.Arguments)
			switch p.Name {
			case "wait":
				waiting = req.ID
				continue
			case "crash":
				os.Exit(7)
			case "fail":
				respond(req.ID, map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": "fixture failed"}}}, nil)
			case "image":
				respond(req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": "DOM snapshot"}, map[string]string{"type": "image", "mimeType": "image/png", "data": "BASE64_MUST_NOT_ESCAPE"}}}, nil)
			case "delegate_task":
				respond(req.ID, map[string]any{"content": []any{}, "structuredContent": map[string]string{"taskId": "task-opaque", "childThreadId": "thread-opaque", "secretRef": "secret-opaque"}}, nil)
			default:
				cwd, _ := os.Getwd()
				respond(req.ID, map[string]any{"content": []any{}, "structuredContent": map[string]any{"arguments": p.Arguments, "cwd": cwd, "argv": os.Args[1:], "token": os.Getenv("FIXTURE_TOKEN"), "providerKey": os.Getenv("ACPLAUSE_API_KEY"), "label": os.Getenv("FIXTURE_LABEL")}}, nil)
			}
		case "ping":
			respond(req.ID, map[string]any{}, nil)
		default:
			respond(req.ID, nil, map[string]any{"code": -32601, "message": "unsupported method"})
		}
	}
}
func respond(id json.RawMessage, result, err any) {
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		m["error"] = err
	} else {
		m["result"] = result
	}
	b, _ := json.Marshal(m)
	fmt.Println(string(b))
}
func record(name string, args json.RawMessage) {
	path := os.Getenv("FIXTURE_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	fmt.Fprintln(f, string(b))
}
