//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadFIFOReportsFailureAndSessionCloses(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("read_file", `{"path":"pipe"}`), reply("Cannot read a pipe."))
	a, sid, cwd := started(t, llm)
	p := filepath.Join(cwd, "pipe")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	keeper, err := os.OpenFile(p, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keeper.Close() })
	a.accepted(a.prompt(sid, "read the pipe"))
	calls := filter(a.untilIdle(sid), "tool_call_update")
	if len(calls) != 2 || calls[1].Status != "failed" || !strings.Contains(calls[1].text(), "regular file") {
		t.Fatalf("FIFO call: %v", calls)
	}
	msgs := llm.request(1).Messages
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), "regular file") {
		t.Fatal("model did not receive the file error")
	}
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
}
