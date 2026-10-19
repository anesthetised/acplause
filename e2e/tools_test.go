package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestToolLoop(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		callTool("read_file", `{"pa|th": "go|.mod"}`),
		reply("It is a Go module."),
	)
	a, sid, cwd := started(t, llm)
	if err := os.WriteFile(filepath.Join(cwd, "go.mod"), []byte("module example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a.accepted(a.prompt(sid, "what is this?"))
	updates := a.untilIdle(sid)
	want := []string{
		"user_message", "state_update running",
		"usage_update", "tool_call_update in_progress", "tool_call_update completed",
		"agent_message_chunk", "usage_update", "state_update idle end_turn",
	}
	if got := kinds(updates); !slices.Equal(got, want) {
		t.Fatalf("updates:\n got %v\nwant %v", got, want)
	}

	calls := filter(updates, "tool_call_update")
	start, done := calls[0], calls[1]
	if start.Name != "read_file" || start.Kind != "read" || start.Title != "Read go.mod" ||
		len(start.Locations) != 1 || start.Locations[0].Path != filepath.Join(cwd, "go.mod") {
		t.Fatalf("tool call start: %#v", start)
	}
	assertJSON(t, start.RawInput, `{"path":"go.mod"}`)
	if done.ToolCallID != start.ToolCallID || done.text() != "module example\n" {
		t.Fatalf("tool call end: %+v", done)
	}

	msgs := llm.request(1).Messages
	assistant, result := msgs[2], msgs[3]
	toolCalls, _ := assistant["tool_calls"].([]any)
	if assistant["role"] != "assistant" || len(toolCalls) != 1 {
		t.Fatalf("assistant message: %v", assistant)
	}
	if result["role"] != "tool" || result["tool_call_id"] != "call_read_file" || result["content"] != "module example\n" {
		t.Fatalf("tool result message: %v", result)
	}
}

func TestToolErrorsGoBackToTheModel(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		callTool("read_file", `{"path":`),
		callTool("read_file", `{"path":"../../etc/passwd"}`),
		callTool("teleport", `{}`),
		reply("I give up."),
	)
	a, sid, _ := started(t, llm)
	a.accepted(a.prompt(sid, "break things"))

	failed := filter(a.untilIdle(sid), "tool_call_update")
	// Calls that cannot start are reported once, already failed.
	if len(failed) != 3 {
		t.Fatalf("tool updates: %+v", failed)
	}
	for i, wantErr := range []string{"invalid arguments", "outside the workspace", `unknown tool "teleport"`} {
		u := failed[i]
		if u.Status != "failed" || u.Name == "" || u.Title == "" || !strings.Contains(u.text(), wantErr) {
			t.Errorf("call %d: %+v", i, u)
		}
		msgs := llm.request(i + 1).Messages
		if got := msgs[len(msgs)-1]["content"].(string); !strings.Contains(got, wantErr) {
			t.Errorf("model was told %q, want %q", got, wantErr)
		}
	}
	if string(failed[0].RawInput) != `"{\"path\":"` {
		t.Errorf("invalid arguments should be reported as a string: %s", failed[0].RawInput)
	}
}

func TestRoundLimit(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("list_dir", `{}`), callTool("list_dir", `{}`))
	a, sid, _ := started(t, llm, withEnv("ACPLAUSE_MAX_ROUNDS=2"))
	a.accepted(a.prompt(sid, "loop forever"))
	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "max_turn_requests" {
		t.Fatalf("idle: %+v", idle)
	}
	if n := llm.requestCount(); n != 2 {
		t.Fatalf("model requests: %d", n)
	}
}

func TestMissingWriteContentPreservesFiles(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		callTool("write_file", `{"path":"a.txt"}`),
		callTool("edit", `{"path":"a.txt","old_text":"keep","new_text":null}`),
		reply("Invalid arguments."),
	)
	a, sid, cwd := started(t, llm)
	p := filepath.Join(cwd, "a.txt")
	if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.accepted(a.prompt(sid, "edit"))
	calls := filter(a.untilIdle(sid), "tool_call_update")
	if len(calls) != 2 {
		t.Fatalf("invalid calls must fail before starting: %v", calls)
	}
	for i, call := range calls {
		if call.Status != "failed" || !strings.Contains(call.text(), "required") {
			t.Errorf("invalid call: %+v", call)
		}
		msgs := llm.request(i + 1).Messages
		if last := msgs[len(msgs)-1]; last["role"] != "tool" || !strings.Contains(last["content"].(string), "required") {
			t.Errorf("missing tool error in history: %v", last)
		}
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != "keep" {
		t.Fatalf("invalid call changed file: %q, %v", data, err)
	}
}

func TestCancelDuringBash(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		callTool("bash", `{"command":"sleep 60 & echo $! > child.pid; wait"}`),
		reply("ok"),
	)
	a, sid, cwd := started(t, llm)
	pidFile := filepath.Join(cwd, "child.pid")

	a.accepted(a.prompt(sid, "run it"))
	start := a.updateWhere(sid, func(u update) bool { return u.is("tool_call_update") })
	if start.Status != "in_progress" || start.Kind != "execute" {
		t.Fatalf("bash start: %+v", start)
	}
	waitForFile(t, pidFile)
	a.notify("session/cancel", map[string]any{"sessionId": sid})

	updates := a.untilIdle(sid)
	want := []string{"tool_call_update cancelled", "state_update idle cancelled"}
	if got := kinds(updates); !slices.Equal(got[len(got)-2:], want) || len(filter(updates, "tool_call_update")) != 1 {
		t.Fatalf("updates after cancel: %v", got)
	}
	assertProcessGone(t, pidFile)

	// The history stays valid: the next request carries a result for the
	// cancelled call.
	a.accepted(a.prompt(sid, "next"))
	a.untilIdle(sid)
	msgs := llm.request(1).Messages
	if result := msgs[3]; result["role"] != "tool" || result["content"] != "Cancelled by the user." {
		t.Fatalf("history after cancel: %v", msgs)
	}
}

func TestAdditionalDirectories(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	shared := t.TempDir()
	lib := filepath.Join(shared, "lib.txt")
	if err := os.WriteFile(lib, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	llm.enqueue(callTool("read_file", `{"path":"`+lib+`"}`), reply("read it"))

	a := start(t, llm)
	a.initialize()
	sid := a.newSession(t.TempDir(), shared).SessionID
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
	a.accepted(a.prompt(sid, "read the shared file"))
	updates := a.untilIdle(sid)
	done := updates[slices.IndexFunc(updates, func(u update) bool { return u.Status == "completed" })]
	if done.text() != "shared" {
		t.Fatalf("read from additional directory: %+v", done)
	}
	if !strings.Contains(llm.request(0).Messages[0]["content"].(string), shared) {
		t.Fatal("system prompt does not mention the additional directory")
	}
}

func filter(updates []update, kind string) []update {
	return slices.DeleteFunc(slices.Clone(updates), func(u update) bool { return !u.is(kind) })
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d started by bash survived cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
