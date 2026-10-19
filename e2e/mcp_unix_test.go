//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMCPSetupCannotSurviveCloseOrDelete(t *testing.T) {
	for _, method := range []string{"session/close", "session/delete"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			a := start(t, llm)
			a.initialize()
			cwd := t.TempDir()
			sid := mcpSession(t, a, cwd)
			// Exercise setup of a stored session, before publication locally.
			a.ok("session/close", map[string]any{"sessionId": sid}, nil)
			gate, pidFile := filepath.Join(cwd, "gate"), filepath.Join(cwd, "pid")
			cfg := mcpConfig("slow", filepath.Join(cwd, "calls"), "candidate")
			cfg["env"] = append(cfg["env"].([]any), map[string]string{"name": "FIXTURE_GATE", "value": gate}, map[string]string{"name": "FIXTURE_PID", "value": pidFile})
			resume := a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{cfg}})
			waitForFile(t, gate+".entered")
			pid := fixturePID(t, pidFile)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			a.ok(method, map[string]any{"sessionId": sid}, nil)
			if r := a.response(resume); r.Error == nil {
				t.Fatal("interrupted setup succeeded")
			}
			waitForMCPExit(t, pid)
			a.fails("session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]string{"type": "text", "text": "must stay closed"}}}, -32002)
		})
	}
}

func fixturePID(t *testing.T, path string) int {
	t.Helper()
	waitForFile(t, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid fixture pid %q", raw)
	}
	return pid
}

func waitForMCPExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("MCP process survived session cleanup")
}
