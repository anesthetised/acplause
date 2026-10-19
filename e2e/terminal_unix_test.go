//go:build unix

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTerminalCancellationAndClose(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel", "close", "delete", "timeout", "signal"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			command := `printf ready; sleep 60`
			if action == "signal" {
				command = `printf ready; kill -TERM $$`
			}
			args := `{"command":` + strconv.Quote(command) + `}`
			llm.enqueue(callTool("bash", args), reply("Done."))
			env := []string{"ACPLAUSE_BASH_TIMEOUT=0"}
			if action == "timeout" {
				env = []string{"ACPLAUSE_BASH_TIMEOUT=1s"}
			}
			a, sid, cwd := started(t, llm, withEnv(env...))
			a.accepted(a.prompt(sid, "run"))
			start := a.updateWhere(sid, func(u update) bool { return u.is("terminal_update") })
			if action != "signal" {
				a.updateWhere(sid, func(u update) bool { return u.is("terminal_output_chunk") })
				switch action {
				case "cancel":
					a.notify("session/cancel", map[string]any{"sessionId": sid})
				case "close", "delete":
					a.ok("session/"+action, map[string]any{"sessionId": sid}, nil)
				}
			}
			updates := a.untilIdle(sid)
			terminals := filter(updates, "terminal_update")
			if len(terminals) != 1 || terminals[0].TerminalID != start.TerminalID || terminals[0].ExitStatus == nil {
				t.Fatalf("terminal end: %+v", terminals)
			}
			final := terminals[0]
			wantSignal := "SIGKILL"
			if action == "signal" {
				wantSignal = "SIGTERM"
			}
			if final.ExitStatus.ExitCode != nil || final.ExitStatus.Signal != wantSignal || final.Output == nil || string(terminalBytes(t, final.Output.Data)) != "ready" {
				t.Fatalf("signal/output: %+v", final)
			}
			calls := filter(updates, "tool_call_update")
			last := calls[len(calls)-1]
			wantStatus, wantStop := "cancelled", "cancelled"
			if action == "signal" || action == "timeout" {
				wantStatus, wantStop = "failed", "end_turn"
			}
			if last.Status != wantStatus || last.seq <= final.seq || updates[len(updates)-1].StopReason != wantStop {
				t.Fatalf("final order/state: %v", kinds(updates))
			}
			for _, u := range filter(updates, "terminal_output_chunk") {
				if u.seq > final.seq {
					t.Fatal("chunk after finalization")
				}
			}
			a.expectQuiet(func(m *message) bool {
				return m.Method == "session/update" && sessionOf(m) == sid && decodeUpdate(t, m).is("terminal_output_chunk")
			})
			if action == "cancel" || action == "close" {
				a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
				replay := a.untilIdle(sid)
				terminals = filter(replay, "terminal_update")
				if len(terminals) != 1 || terminals[0].ExitStatus == nil || terminals[0].ExitStatus.Signal != "SIGKILL" {
					t.Fatalf("cancelled replay: %+v", terminals)
				}
				assertTerminalLink(t, filter(replay, "tool_call_update")[0], start.TerminalID)
			}
		})
	}
}

func TestTerminalCrashRecoversUnknownExit(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("bash", `{"command":"echo $$ > pid; printf ready; sleep 60"}`), reply("Recovered."))
	a, sid, cwd := started(t, llm, withDataDir(data))
	a.accepted(a.prompt(sid, "run"))
	running := a.updateWhere(sid, func(u update) bool { return u.is("terminal_update") })
	a.updateWhere(sid, func(u update) bool { return u.is("terminal_output_chunk") })
	raw, err := os.ReadFile(filepath.Join(cwd, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid %q, %v", raw, err)
	}
	defer syscall.Kill(-pid, syscall.SIGKILL)
	// Force an existing save boundary while streaming so recovery must retain
	// these bytes, rather than only the empty tool-start snapshot.
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{t.TempDir()}}, nil)
	db, err := sql.Open("sqlite", filepath.Join(data, "acplause.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var encoded string
		err := db.QueryRow(`SELECT json_extract(data, '$.update.output.data') FROM transcript WHERE session_id=? AND json_extract(data, '$.kind')='terminal_update'`, sid).Scan(&encoded)
		if err == nil && encoded == "cmVhZHk=" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal snapshot was not saved: %q, %v", encoded, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.kill()
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET lease_until=0 WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	b := start(t, llm, withDataDir(data))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	updates := b.untilIdle(sid)
	terminals := filter(updates, "terminal_update")
	if len(terminals) != 1 || terminals[0].TerminalID != running.TerminalID || terminals[0].ExitStatus == nil || terminals[0].ExitStatus.ExitCode != nil || terminals[0].ExitStatus.Signal != "" || string(terminalBytes(t, terminals[0].Output.Data)) != "ready" {
		t.Fatalf("recovered terminal: %+v", terminals)
	}
	calls := filter(updates, "tool_call_update")
	if len(calls) != 1 || calls[0].Status != "failed" || !strings.Contains(calls[0].text(), "exited before") {
		t.Fatalf("recovered call: %+v", calls)
	}
	assertTerminalLink(t, calls[0], running.TerminalID)
	b.accepted(b.prompt(sid, "continue"))
	b.untilIdle(sid)
	count := 0
	for _, m := range llm.request(1).Messages {
		if m["role"] == "tool" && m["tool_call_id"] == "call_bash" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recovery tool results: %d", count)
	}
}
