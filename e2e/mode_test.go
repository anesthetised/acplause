package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// setMode changes the mode, which an idle session accepts at once, even
// while the finished turn is still being saved.
func setMode(t *testing.T, a *agent, sid, mode string) []configOption {
	t.Helper()
	response := a.response(a.request("session/set_config_option", modeRequest(sid, mode)))
	if response.Error != nil {
		t.Fatalf("set mode: %+v", response.Error)
	}
	var result struct{ ConfigOptions []configOption }
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result.ConfigOptions
}

func modeRequest(sid, value string) map[string]any {
	return map[string]any{"sessionId": sid, "configId": "mode", "type": "id", "value": value}
}

func TestModePlanBuildPolicyAndRestart(t *testing.T) {
	t.Parallel()
	data, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("Keep project instructions."), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(cwd, "mcp-calls")
	cfg := mcpConfig("external", log, "mode")
	llm := newMockLLM(t)
	llm.enqueue(reply("discovered"))
	a := start(t, llm, withDataDir(data), askingUser)
	a.initialize()
	sid := mcpSession(t, a, cwd, cfg)
	a.accepted(a.prompt(sid, "discover"))
	a.untilIdle(sid)
	external := mcpName(t, llm.request(0), "external", "read_file")
	options := setMode(t, a, sid, "plan")
	mode := configByID(t, options, "mode")
	if mode.Category != "mode" || mode.CurrentValue != "plan" || !slices.Equal(values(mode), []string{"build", "plan"}) {
		t.Fatalf("mode selector: %+v", mode)
	}
	a.fails("session/set_config_option", modeRequest(sid, "_unknown"), -32602)
	llm.enqueue(func(s *stream) {
		// Even the stale external read_file claims readOnlyHint in this fixture.
		for i, call := range []struct{ name, args string }{
			{testShellName(), testShellArgs(`cat AGENTS.md; touch changed`, `[Console]::Write([IO.File]::ReadAllText('AGENTS.md')); [IO.File]::WriteAllText('changed', '')`)},
			{"write_file", `{"path":"changed","content":"bad"}`},
			{"edit", `{"path":"AGENTS.md","old_text":"Keep","new_text":"Lose"}`},
			{external, `{}`},
		} {
			s.toolCall(i, call.name, call.name, call.args)
		}
		s.finish("tool_calls")
	}, callTool("read_file", `{"path":"AGENTS.md"}`), callTool("list_dir", `{}`), callTool("search_code", `{"pattern":"Keep"}`), callTool("update_plan", initialPlan), reply("Proposed."))
	a.accepted(a.prompt(sid, "inspect and plan"))
	updates := a.untilIdle(sid)
	failed := 0
	for _, call := range filter(updates, "tool_call_update") {
		if call.Status == "failed" {
			failed++
			if !strings.Contains(call.text(), "Plan mode") {
				t.Fatalf("unexpected failure: %+v", call)
			}
		}
	}
	if failed != 4 || len(filter(updates, "plan_update")) != 1 {
		t.Fatalf("Plan policy: %v", kinds(updates))
	}
	req := llm.request(1)
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool["function"].(map[string]any)["name"].(string))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"get_instructions", "list_dir", "read_file", "search_code", "update_plan"}) {
		t.Fatalf("Plan tools: %v", names)
	}
	for _, part := range []string{"Plan mode", "Do not claim", cwd, "Keep project instructions."} {
		if !strings.Contains(req.Messages[0]["content"].(string), part) {
			t.Fatalf("prompt lost %q", part)
		}
	}
	results := llm.request(2).Messages
	if len(results) != 9 {
		t.Fatalf("missing tool results: %+v", results)
	}
	for _, result := range results[len(results)-4:] {
		if result["role"] != "tool" || !strings.Contains(result["content"].(string), "Plan mode") {
			t.Fatalf("forbidden result: %+v", result)
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, "changed")); !os.IsNotExist(err) {
		t.Fatalf("Plan changed workspace: %v", err)
	}
	if contents, _ := os.ReadFile(log); len(contents) != 0 {
		t.Fatalf("Plan dispatched MCP: %s", contents)
	}
	a.expectQuiet(func(m *message) bool {
		return m.Method == "session/request_permission" || (m.Method == "session/update" && strings.Contains(string(m.Params), "config_option_update"))
	})
	a.stop()

	b := start(t, llm, withDataDir(data), askingUser)
	b.initialize()
	var resumed struct{ ConfigOptions []configOption }
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{cfg}}, &resumed)
	b.untilIdle(sid)
	if configByID(t, resumed.ConfigOptions, "mode").CurrentValue != "plan" {
		t.Fatalf("lost mode: %+v", resumed)
	}
	fresh := b.newSession(cwd)
	if configByID(t, fresh.ConfigOptions, "mode").CurrentValue != "build" {
		t.Fatal("new session inherited Plan")
	}
	// The restored system prompt must retain Plan before switching back.
	llm.enqueue(reply("still planning"))
	b.accepted(b.prompt(sid, "continue planning"))
	b.untilIdle(sid)
	if !strings.Contains(llm.request(7).Messages[0]["content"].(string), "Plan mode") {
		t.Fatal("restored system prompt lost Plan")
	}
	setMode(t, b, sid, "build")
	llm.enqueue(callTool("write_file", `{"path":"changed","content":"allowed"}`), reply("Built."))
	b.accepted(b.prompt(sid, "build"))
	permission := b.permissionRequest()
	b.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
	b.untilIdle(sid)
	if content, err := os.ReadFile(filepath.Join(cwd, "changed")); err != nil || string(content) != "allowed" {
		t.Fatalf("Build write: %q %v", content, err)
	}
	if strings.Contains(llm.request(8).Messages[0]["content"].(string), "Plan mode") {
		t.Fatal("Build kept Plan instructions")
	}
	// The MCP connection remains usable and still requires explicit permission.
	llm.enqueue(mcpCall(t, "external", "read_file", `{}`), reply("MCP restored."))
	b.accepted(b.prompt(sid, "external read"))
	permission = b.permissionRequest()
	b.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
	b.untilIdle(sid)
	if content, err := os.ReadFile(log); err != nil || len(content) == 0 {
		t.Fatalf("Build MCP did not run: %q %v", content, err)
	}
}

func TestModeRefusesPendingPermissionAndQueuedPrompt(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("write_file", `{"path":"approved","content":"done"}`), reply("done"), reply("queued"))
	a, sid, cwd := started(t, llm, askingUser)
	a.accepted(a.prompt(sid, "write"))
	permission := a.permissionRequest()
	err := a.fails("session/set_config_option", modeRequest(sid, "plan"), -32602)
	if !strings.Contains(err.Message, "retry") {
		t.Fatalf("busy guidance: %+v", err)
	}
	a.ok("session/set_config_option", modeRequest(sid, "build"), nil)
	queued := a.prompt(sid, "queued")
	a.fails("session/set_config_option", modeRequest(sid, "plan"), -32602)
	a.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
	first := a.untilIdle(sid)
	a.accepted(queued)
	second := a.untilIdle(sid)
	for _, updates := range [][]update{first, second} {
		idle := 0
		for _, u := range filter(updates, "state_update") {
			if u.State == "idle" {
				idle++
				if u.StopReason != "end_turn" {
					t.Fatalf("turn failed: %+v", u)
				}
			}
		}
		if idle != 1 {
			t.Fatalf("idle count: %d", idle)
		}
	}
	if content, err := os.ReadFile(filepath.Join(cwd, "approved")); err != nil || string(content) != "done" {
		t.Fatalf("pending approval changed behavior: %q %v", content, err)
	}
	setMode(t, a, sid, "plan")
}

func TestModePlanQuestionsNeverAuthorizeExecution(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("ask_user", formArgs), callTool(testShellName(), testShellArgs(`touch forbidden`, `[IO.File]::WriteAllText('forbidden', '')`)), reply("Proposed."))
	a, sid, cwd := startedForms(t, llm, askingUser)
	setMode(t, a, sid, "plan")
	a.accepted(a.prompt(sid, "ask before planning"))
	a.answer(a.elicitationRequest(), formAnswer("accept"))
	updates := a.untilIdle(sid)
	calls := filter(updates, "tool_call_update")
	if len(calls) != 3 || calls[1].Status != "completed" || calls[2].Name != testShellName() || calls[2].Status != "failed" {
		t.Fatalf("Plan form flow: %v", kinds(updates))
	}
	if !slices.ContainsFunc(llm.request(0).Tools, func(tool map[string]any) bool { return tool["function"].(map[string]any)["name"] == "ask_user" }) {
		t.Fatal("Plan did not offer questions")
	}
	if _, err := os.Stat(filepath.Join(cwd, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("form authorized execution: %v", err)
	}
	a.expectQuiet(func(m *message) bool { return m.Method == "session/request_permission" })
}
