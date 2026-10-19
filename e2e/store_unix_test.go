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

	_ "modernc.org/sqlite" // expires a dead agent's lease without waiting 30 s
)

// kill bypasses graceful shutdown to exercise crash recovery.
func (a *agent) kill() {
	a.t.Helper()
	a.stopOnce.Do(func() {
		if err := a.cmd.Process.Kill(); err != nil {
			a.t.Fatal(err)
		}
		_ = a.stdin.Close()
		_ = a.cmd.Wait()
		<-a.readerDone
	})
}

func TestSessionRecoversAfterCrash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(callTool("bash", `{"command":"echo $$ > pid; sleep 30"}`), reply("recovered"))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	a.accepted(a.prompt(sid, "run"))
	call := a.toolStarted(sid)
	pidFile := filepath.Join(cwd, "pid")
	waitForFile(t, pidFile)
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid bash pid %q: %v", raw, err)
	}
	defer syscall.Kill(-pid, syscall.SIGKILL)
	a.kill()
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE sessions SET lease_until = 0 WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}

	b := start(t, llm, withDataDir(dir))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := b.untilIdle(sid)
	calls := filter(replay, "tool_call_update")
	const interrupted = "acplause exited before this tool call finished."
	if len(calls) != 1 || calls[0].ToolCallID != call.ToolCallID || calls[0].Status != "failed" || calls[0].text() != interrupted {
		t.Fatalf("recovered tool: %+v", calls)
	}
	b.accepted(b.prompt(sid, "continue"))
	b.untilIdle(sid)
	var results int
	for _, m := range llm.request(1).Messages {
		if m["role"] == "tool" && m["tool_call_id"] == "call_bash" {
			results++
			if m["content"] != interrupted {
				t.Fatalf("recovered model result: %+v", m)
			}
		}
	}
	if results != 1 {
		t.Fatalf("got %d recovered tool results", results)
	}
}
