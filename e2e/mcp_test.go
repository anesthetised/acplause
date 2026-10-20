package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPReadOnlyHintStillRequiresPermission(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "alpha", "read_file", `{}`), reply("declined"))
	a := start(t, llm, askingUser)
	a.initialize()
	cwd := t.TempDir()
	log := filepath.Join(cwd, "calls")
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", log, "first"))
	a.accepted(a.prompt(sid, "call"))
	p := a.permissionRequest()
	a.answer(p, selected(optionOfKind(t, p, "reject_once")))
	updates := a.untilIdle(sid)
	if data, _ := os.ReadFile(log); len(data) != 0 {
		t.Fatalf("declined tool dispatched: %s", data)
	}
	failed := false
	for _, u := range updates {
		failed = failed || u.Status == "failed"
	}
	if !failed || !strings.Contains(llm.request(1).Messages[len(llm.request(1).Messages)-1]["content"].(string), "reject") {
		t.Fatal("permission rejection did not reach model and ACP")
	}
}

func mcpConfig(name, log, label string) map[string]any {
	return map[string]any{"type": "stdio", "name": name, "command": mcpBinary, "args": []string{"fixture-argument"}, "env": []any{map[string]string{"name": "FIXTURE_LOG", "value": log}, map[string]string{"name": "FIXTURE_TOKEN", "value": "configured-token"}, map[string]string{"name": "FIXTURE_LABEL", "value": label}}}
}
func mcpName(t *testing.T, req chatRequest, server, name string) string {
	t.Helper()
	for _, tool := range req.Tools {
		f, _ := tool["function"].(map[string]any)
		n, _ := f["name"].(string)
		if f["description"] == "fixture:"+name && strings.HasPrefix(n, "mcp_"+server+"_") {
			return n
		}
	}
	t.Fatalf("MCP tool %s/%s missing from %+v", server, name, req.Tools)
	return ""
}
func mcpCall(t *testing.T, server, name, args string) script {
	return func(s *stream) {
		s.toolCall(0, "mcp-call-"+name, mcpName(t, s.req, server, name), args)
		s.finish("tool_calls")
	}
}
func mcpSession(t *testing.T, a *agent, cwd string, configs ...any) string {
	t.Helper()
	var res newSessionResult
	a.ok("session/new", map[string]any{"cwd": cwd, "mcpServers": configs}, &res)
	a.ok("session/set_config_option", map[string]any{"sessionId": res.SessionID, "configId": "model", "type": "id", "value": "test/model"}, nil)
	return res.SessionID
}

func TestMCPStdioRelay(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	args := `{"schedule":{"type":"interval","everyMs":3600000},"nested":[{"id":"opaque"}],"large":9007199254740993}`
	llm.enqueue(mcpCall(t, "alpha", "schedule_task", args), mcpCall(t, "alpha", "delegate_task", `{}`), mcpCall(t, "beta", "read_file", `{}`), reply("done"))
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	log := filepath.Join(cwd, "calls")
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", log, "alpha"), mcpConfig("beta", log, "beta"))
	a.accepted(a.prompt(sid, "schedule and delegate"))
	updates := a.untilIdle(sid)
	msgs := llm.request(1).Messages
	text := msgs[len(msgs)-1]["content"].(string)
	for _, want := range []string{`"schedule":{"type":"interval","everyMs":3600000}`, `9007199254740993`, cwd, `configured-token`, `fixture-argument`, `"providerKey":""`} {
		if !strings.Contains(text, want) {
			t.Errorf("result lacks %q: %s", want, text)
		}
	}
	msgs = llm.request(2).Messages
	text = msgs[len(msgs)-1]["content"].(string)
	for _, want := range []string{"task-opaque", "thread-opaque", "secret-opaque"} {
		if !strings.Contains(text, want) {
			t.Errorf("lost reference %s", want)
		}
	}
	calls := filter(updates, "tool_call_update")
	pending := 0
	for _, c := range calls {
		if c.Status == "pending" {
			pending++
		}
	}
	if pending != 3 {
		t.Fatalf("readOnlyHint bypassed permission: %v", kinds(updates))
	}
	if len(llm.request(0).Tools) != 23 {
		t.Fatalf("pagination or collision lost tools: %d", len(llm.request(0).Tools))
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), `"name"`) != 3 {
		t.Fatalf("routing: %s", data)
	}
}

func TestMCPFailuresAndImages(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(mcpCall(t, "alpha", "fail", `{}`), mcpCall(t, "alpha", "read_file", `[]`), mcpCall(t, "alpha", "image", `{}`), mcpCall(t, "alpha", "crash", `{}`), reply("done"))
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	sid := mcpSession(t, a, cwd, mcpConfig("alpha", filepath.Join(cwd, "calls"), "alpha"))
	a.accepted(a.prompt(sid, "exercise failures"))
	updates := a.untilIdle(sid)
	failed := 0
	for _, u := range updates {
		if u.Status == "failed" {
			failed++
		}
		if strings.Contains(u.text(), "BASE64_MUST_NOT_ESCAPE") {
			t.Fatal("image bytes leaked")
		}
	}
	if failed != 3 {
		t.Fatalf("failures: %v", kinds(updates))
	}
	for i := 1; i <= 4; i++ {
		raw, _ := json.Marshal(llm.request(i))
		if strings.Contains(string(raw), "BASE64_MUST_NOT_ESCAPE") {
			t.Fatal("image bytes reached model")
		}
	}
	msgs := llm.request(3).Messages
	text := msgs[len(msgs)-1]["content"].(string)
	if !strings.Contains(text, "DOM snapshot") || !strings.Contains(text, "Unsupported MCP") {
		t.Fatalf("missing image marker: %s", text)
	}
}
