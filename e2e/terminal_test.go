package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type terminalOutput struct {
	Data string `json:"data"`
}
type terminalExit struct {
	ExitCode *uint32 `json:"exitCode"`
	Signal   string  `json:"signal"`
}

func terminalBytes(t *testing.T, data string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("invalid terminal base64 %q: %v", data, err)
	}
	return b
}

func assertTerminalLink(t *testing.T, u update, id string) {
	t.Helper()
	var content []struct {
		Type       string
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(u.Content, &content); err != nil || len(content) == 0 || content[0].Type != "terminal" || content[0].TerminalID != id {
		t.Fatalf("missing terminal link: %s, %v", u.Content, err)
	}
}

func TestTerminalLiveResumeAndRestart(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	const command = `printf '\342'; while [ ! -f release ]; do sleep 0.01; done; printf '\202\254\000\377' >&2`
	args, _ := json.Marshal(map[string]string{"command": command})
	llm.enqueue(callTool("bash", string(args)), reply("Done."))
	data := t.TempDir()
	a, sid, cwd := started(t, llm, withDataDir(data))
	a.accepted(a.prompt(sid, "run"))
	running := a.updateWhere(sid, func(u update) bool { return u.is("terminal_update") })
	tool := a.toolStarted(sid)
	if running.TerminalID == "" || running.Command != command || running.Cwd != cwd || running.ExitStatus != nil || running.seq >= tool.seq {
		t.Fatalf("start: %+v; tool: %+v", running, tool)
	}
	assertTerminalLink(t, tool, running.TerminalID)
	chunk := a.updateWhere(sid, func(u update) bool { return u.is("terminal_output_chunk") })
	if chunk.TerminalID != running.TerminalID || !bytes.Equal(terminalBytes(t, chunk.Data), []byte{0xe2}) || chunk.seq <= tool.seq {
		t.Fatalf("live: %+v", chunk)
	}
	params := map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}
	response := a.ok("session/resume", params, nil)
	replay := a.updateWhere(sid, func(u update) bool { return u.is("terminal_update") })
	if replay.TerminalID != running.TerminalID || replay.Output == nil || replay.Output.Data != chunk.Data || replay.ExitStatus != nil || replay.seq >= response.seq {
		t.Fatalf("live replay: %+v", replay)
	}
	// Consume the replayed running state before releasing the command.
	a.updateWhere(sid, func(u update) bool { return u.is("state_update") && u.State == "running" && u.seq > replay.seq })
	if err := os.WriteFile(filepath.Join(cwd, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	updates := a.untilIdle(sid)
	terminals := filter(updates, "terminal_update")
	if len(terminals) != 1 {
		t.Fatalf("final terminals: %+v", terminals)
	}
	final := terminals[0]
	want := []byte{0xe2, 0x82, 0xac, 0, 0xff}
	if final.TerminalID != running.TerminalID || final.Output == nil || !bytes.Equal(terminalBytes(t, final.Output.Data), want) || final.ExitStatus == nil || final.ExitStatus.ExitCode == nil || *final.ExitStatus.ExitCode != 0 || final.ExitStatus.Signal != "" {
		t.Fatalf("final: %+v", final)
	}
	calls := filter(updates, "tool_call_update")
	completed := calls[len(calls)-1]
	if completed.Status != "completed" || completed.seq <= final.seq {
		t.Fatalf("completion: %+v, final: %+v", completed, final)
	}
	assertTerminalLink(t, completed, running.TerminalID)
	for _, u := range filter(updates, "terminal_output_chunk") {
		if u.seq > final.seq || len(terminalBytes(t, u.Data)) > 16*1024 {
			t.Fatalf("late/oversize chunk: %+v", u)
		}
	}
	req := llm.request(1)
	if result := req.Messages[len(req.Messages)-1]; result["role"] != "tool" || result["content"] != "€\x00�" {
		t.Fatalf("model result: %+v", result)
	}
	a.stop()
	b := start(t, llm, withDataDir(data))
	b.initialize()
	b.ok("session/resume", params, nil)
	restored := b.untilIdle(sid)
	snapshots := filter(restored, "terminal_update")
	if len(snapshots) != 1 || !reflect.DeepEqual(snapshots[0].Output, final.Output) || !reflect.DeepEqual(snapshots[0].ExitStatus, final.ExitStatus) || len(filter(restored, "terminal_output_chunk")) != 0 {
		t.Fatalf("restart: %+v", restored)
	}
	restoredTools := filter(restored, "tool_call_update")
	if len(restoredTools) != 1 {
		t.Fatalf("replayed tool count: %d", len(restoredTools))
	}
	assertTerminalLink(t, restoredTools[0], running.TerminalID)
}

func TestTerminalBoundedReplacement(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("bash", `{"command":"printf '%100000s' ''; while [ ! -f release ]; do sleep 0.01; done; exit 9"}`), reply("Failed."))
	a, sid, cwd := started(t, llm)
	a.accepted(a.prompt(sid, "run"))
	streamed := 0
	for streamed < 50000 {
		chunk := a.updateWhere(sid, func(u update) bool { return u.is("terminal_output_chunk") })
		n := len(terminalBytes(t, chunk.Data))
		if n == 0 || n > 16*1024 {
			t.Fatalf("chunk size %d", n)
		}
		streamed += n
	}
	if streamed != 50000 {
		t.Fatalf("unbounded stream: %d", streamed)
	}
	if err := os.WriteFile(filepath.Join(cwd, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	updates := a.untilIdle(sid)
	terminals := filter(updates, "terminal_update")
	if len(terminals) != 2 {
		t.Fatalf("terminal lifecycle: %+v", terminals)
	}
	final := terminals[1]
	want := strings.Repeat(" ", 50000) + "\n... (50000 more bytes of output dropped)"
	if final.Output == nil || string(terminalBytes(t, final.Output.Data)) != want || final.ExitStatus == nil || final.ExitStatus.ExitCode == nil || *final.ExitStatus.ExitCode != 9 {
		t.Fatalf("bounded output/exit: %+v", final.ExitStatus)
	}
	for _, u := range filter(updates, "terminal_output_chunk") {
		n := len(terminalBytes(t, u.Data))
		if n > 16*1024 {
			t.Fatalf("chunk size %d", n)
		}
		streamed += n
	}
	if streamed > 50000 {
		t.Fatalf("unbounded stream: %d", streamed)
	}
	calls := filter(updates, "tool_call_update")
	if last := calls[len(calls)-1]; last.Status != "failed" || last.seq <= final.seq || last.text() != want+"\nError: exit status 9" {
		t.Fatalf("tool result: %+v", last)
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := a.untilIdle(sid)
	snapshots := filter(replay, "terminal_update")
	if len(snapshots) != 1 || snapshots[0].Output.Data != final.Output.Data || len(filter(replay, "terminal_output_chunk")) != 0 {
		t.Fatalf("bounded replay: %+v", snapshots)
	}
}

func TestTerminalDeniedAndStartFailed(t *testing.T) {
	t.Parallel()
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{true: "denied", false: "start failed"}[denied], func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("bash", `{"command":"touch ran"}`), reply("Could not run."))
			a, sid, cwd := started(t, llm, askingUser)
			a.accepted(a.prompt(sid, "run"))
			req := a.permissionRequest()
			option := "allow_once"
			if denied {
				option = "reject_once"
			} else if err := os.Remove(cwd); err != nil {
				t.Fatal(err)
			}
			a.answer(req, selected(optionOfKind(t, req, option)))
			updates := a.untilIdle(sid)
			if len(filter(updates, "terminal_update")) != 0 || len(filter(updates, "terminal_output_chunk")) != 0 {
				t.Fatalf("phantom terminal: %+v", updates)
			}
			calls := filter(updates, "tool_call_update")
			if len(calls) != 2 || calls[1].Status != "failed" {
				t.Fatalf("failed start: %+v", calls)
			}
			if _, err := os.Stat(filepath.Join(cwd, "ran")); !os.IsNotExist(err) {
				t.Fatalf("command ran: %v", err)
			}
		})
	}
}
