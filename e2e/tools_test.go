package e2e

import (
	"encoding/json"
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
		"user_message", "session_info_update", "state_update running",
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

func TestSearchMatchingOptionsInPlanMode(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		func(s *stream) {
			// callTool uses | to delimit SSE fragments, so send alternation directly.
			s.toolCall(0, "call_search_code", "search_code", `{"pattern":"^(todo|fixme):","regex":true,"ignore_case":true,"file_glob":"*.txt"}`)
			s.finish("tool_calls")
		},
		callTool("search_code", `{"pattern":"k.\u00e9","ignore_case":true}`),
		callTool("search_code", `{"pattern":"[","regex":true}`),
		callTool("search_code", `{"pattern":"todo","ignore_case":"true"}`),
		reply("Found the matching lines; invalid options failed."),
	)
	a, sid, cwd := started(t, llm, askingUser)
	if err := os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("TODO: first\nfixme: second\nK.\u00c9\nKX\u00c9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setMode(t, a, sid, "plan")
	a.accepted(a.prompt(sid, "inspect matching lines"))
	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "end_turn" {
		t.Fatalf("idle: %+v", idle)
	}
	var done []update
	for _, u := range filter(updates, "tool_call_update") {
		if u.Status == "completed" || u.Status == "failed" {
			done = append(done, u)
		}
	}
	if len(done) != 4 {
		t.Fatalf("tool results: %+v", done)
	}
	for i, want := range []struct{ status, text string }{
		{"completed", "notes.txt:1:TODO: first\nnotes.txt:2:fixme: second\n"},
		{"completed", "notes.txt:3:K.\u00c9\n"},
		{"failed", "invalid regex"},
		{"failed", "invalid arguments"},
	} {
		got := done[i]
		if got.Status != want.status || (want.status == "completed" && got.text() != want.text) || !strings.Contains(got.text(), want.text) {
			t.Errorf("call %d: %+v, want %+v", i, got, want)
		}
		msgs := llm.request(i + 1).Messages
		result := msgs[len(msgs)-1]
		if result["role"] != "tool" || result["tool_call_id"] != "call_search_code" || result["content"] != got.text() {
			t.Errorf("call %d history: %v", i, result)
		}
	}
	for _, tool := range llm.request(0).Tools {
		fn := tool["function"].(map[string]any)
		if fn["name"] != "search_code" {
			continue
		}
		props := fn["parameters"].(map[string]any)["properties"].(map[string]any)
		for _, option := range []struct{ name, kind string }{
			{"regex", "boolean"}, {"ignore_case", "boolean"},
			{"offset", "integer"}, {"limit", "integer"}, {"context_lines", "integer"},
		} {
			prop, ok := props[option.name].(map[string]any)
			if !ok || prop["type"] != option.kind {
				t.Errorf("missing %s option %s: %v", option.kind, option.name, props)
			}
		}
		return
	}
	t.Fatal("search_code was not advertised")
}

func TestSearchPaginationAndContextInPlanMode(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		callTool("search_code", `{"pattern":"^hit","regex":true,"ignore_case":true,"limit":2,"context_lines":1}`),
		callTool("search_code", `{"pattern":"^hit","regex":true,"ignore_case":true,"offset":2,"limit":2,"context_lines":1}`),
		callTool("search_code", `{"pattern":"^hit","regex":true,"ignore_case":true,"offset":3}`),
		callTool("search_code", `{"pattern":"hit","offset":null}`),
		reply("Inspected both pages and their context."),
	)
	a, sid, cwd := started(t, llm, askingUser)
	for name, content := range map[string]string{
		"a.txt": "before\nHIT one\nbetween\nhit two\nend\n",
		"b.txt": "lead\nhit three\ntail",
	} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setMode(t, a, sid, "plan")
	a.accepted(a.prompt(sid, "inspect matches with context"))
	updates := a.untilIdle(sid)
	var done []update
	for _, u := range filter(updates, "tool_call_update") {
		if u.Status == "completed" || u.Status == "failed" {
			done = append(done, u)
		}
	}
	if len(done) != 4 || updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("search updates: %v", kinds(updates))
	}
	for i, want := range []string{
		"a.txt-1-before\na.txt:2:HIT one\na.txt-3-between\na.txt:4:hit two\na.txt-5-end\n... more matches; next offset: 2\n",
		"b.txt-1-lead\nb.txt:2:hit three\nb.txt-3-tail\n... no more matches\n",
		`No more matches for "^hit" at offset 3`,
		"invalid arguments",
	} {
		status := "completed"
		if i == 3 {
			status = "failed"
		}
		if done[i].Status != status || (i < 3 && done[i].text() != want) || !strings.Contains(done[i].text(), want) {
			t.Errorf("call %d: %q (%s), want %q (%s)", i, done[i].text(), done[i].Status, want, status)
		}
		msgs := llm.request(i + 1).Messages
		result := msgs[len(msgs)-1]
		if result["role"] != "tool" || result["tool_call_id"] != "call_search_code" || result["content"] != done[i].text() {
			t.Errorf("call %d history: %v", i, result)
		}
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

func TestUnlimitedRounds(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	for range 26 { // exceed the former default limit as well as zero
		llm.enqueue(callTool("list_dir", `{}`))
	}
	llm.enqueue(reply("done"))
	a, sid, _ := started(t, llm, withEnv("ACPLAUSE_MAX_ROUNDS=0"))
	a.accepted(a.prompt(sid, "keep working"))
	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "end_turn" {
		t.Fatalf("idle: %+v", idle)
	}
	if n := llm.requestCount(); n != 27 {
		t.Fatalf("model requests: %d", n)
	}
}

func TestUnlimitedBashTimeout(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("bash", `{"command":"printf done"}`), reply("finished"))
	a, sid, _ := started(t, llm, withEnv("ACPLAUSE_BASH_TIMEOUT=0"))
	a.accepted(a.prompt(sid, "run"))
	updates := a.untilIdle(sid)
	calls := filter(updates, "tool_call_update")
	completed := calls[len(calls)-1]
	if completed.Status != "completed" || completed.text() != "done" || updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("bash: %+v; updates: %v", completed, kinds(updates))
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
	a, sid, cwd := started(t, llm, withEnv("ACPLAUSE_BASH_TIMEOUT=0", "ACPLAUSE_MAX_ROUNDS=0"))
	pidFile := filepath.Join(cwd, "child.pid")

	a.accepted(a.prompt(sid, "run it"))
	a.toolStarted(sid)
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

type diffBlock struct {
	Type    string
	Changes []struct{ Operation, Path, FileType string }
	Patch   *struct{ Format, Text string }
}

// diffContent returns the diff blocks of a tool call update.
func diffContent(t *testing.T, u update) (diffs []diffBlock) {
	t.Helper()
	var blocks []diffBlock
	if err := json.Unmarshal(u.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if b.Type == "diff" {
			diffs = append(diffs, b)
		}
	}
	return diffs
}

func TestFileToolsReportDiffs(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		callTool("edit", `{"path":"a.txt","old_text":"two","new_text":"TWO"}`),
		callTool("write_file", `{"path":"a.txt","content":"one\nTWO\nthree\nfour\n"}`),
		callTool("write_file", `{"path":"sub/b.txt","content":"new\n"}`),
		callTool("edit", `{"path":"a.txt","old_text":"nope","new_text":"x"}`),
		reply("Done."),
	)
	a, sid, cwd := started(t, llm)
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.accepted(a.prompt(sid, "edit"))
	var done []update
	for _, u := range filter(a.untilIdle(sid), "tool_call_update") {
		if u.Status == "completed" || u.Status == "failed" {
			done = append(done, u)
		}
	}
	if len(done) != 4 {
		t.Fatalf("tool calls: %v", done)
	}
	abs := filepath.Join(cwd, "a.txt")
	want := []struct{ op, path, patch string }{
		{"modify", abs, "diff --git " + abs + " " + abs + "\n--- " + abs + "\n+++ " + abs + "\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n"},
		{"modify", abs, "diff --git " + abs + " " + abs + "\n--- " + abs + "\n+++ " + abs + "\n@@ -1,3 +1,4 @@\n one\n TWO\n three\n+four\n"},
		{"add", filepath.Join(cwd, "sub", "b.txt"), ""},
	}
	for i, w := range want {
		diffs := diffContent(t, done[i])
		if done[i].Status != "completed" || len(diffs) != 1 || len(diffs[0].Changes) != 1 {
			t.Fatalf("call %d: %+v", i, done[i])
		}
		c := diffs[0].Changes[0]
		if c.Operation != w.op || c.Path != w.path || c.FileType != "text" {
			t.Errorf("call %d change: %+v", i, c)
		}
		if w.patch != "" && (diffs[0].Patch == nil || diffs[0].Patch.Format != "git_patch" || diffs[0].Patch.Text != w.patch) {
			t.Errorf("call %d patch: %+v", i, diffs[0].Patch)
		}
		if i == 2 && (diffs[0].Patch == nil || !strings.Contains(diffs[0].Patch.Text, "new file mode 100644\n--- /dev/null\n")) {
			t.Errorf("add patch: %+v", diffs[0].Patch)
		}
		if !strings.Contains(done[i].text(), abs[:len(cwd)]) {
			t.Errorf("call %d lost its text result: %q", i, done[i].text())
		}
	}
	if failed := done[3]; failed.Status != "failed" || len(diffContent(t, failed)) != 0 {
		t.Errorf("failed edit must not report a diff: %+v", failed)
	}
}

func TestAgentsMDJoinsTheSystemPrompt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content string
		want          bool
	}{
		{"present", "Always use tabs.\n", true},
		{"oversized", strings.Repeat("x", 64<<10), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(reply("ok"))
			a := start(t, llm)
			a.initialize()
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			sid := a.newSession(cwd).SessionID
			a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
			a.accepted(a.prompt(sid, "hi"))
			a.untilIdle(sid)
			system, _ := llm.request(0).Messages[0]["content"].(string)
			if got := strings.Contains(system, "Project instructions (AGENTS.md)"); got != tc.want || got != strings.Contains(system, strings.TrimSpace(tc.content)) {
				t.Fatalf("instructions in system prompt = %v, want %v:\n%s", got, tc.want, system)
			}
		})
	}
}
