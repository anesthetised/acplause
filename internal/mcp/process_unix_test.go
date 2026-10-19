//go:build unix

package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMCPBlockedWriteKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child")
	p := newPeer(context.Background())
	tr, err := startStdio(p, Config{Command: "/bin/sh", Args: []string{"-c", `trap '' TERM; sleep 120 & echo $! > "$CHILD_PID"; wait`}, Env: map[string]string{"CHILD_PID": childFile}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	p.t = tr
	tr.start()
	defer tr.Close()
	deadline := time.Now().Add(3 * time.Second)
	var child int
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(childFile)
		child, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if child > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if child <= 0 {
		t.Fatal("child did not start")
	}
	defer syscall.Kill(child, syscall.SIGKILL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var result any
	if err = p.call(ctx, "tools/call", map[string]string{"padding": strings.Repeat("x", 1<<20)}, &result); err == nil {
		t.Fatal("non-reading server answered")
	}
	closed := make(chan struct{})
	go func() { _ = tr.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(4 * time.Second):
		t.Fatal("blocked write prevented process cleanup")
	}
	if err := syscall.Kill(tr.cmd.Process.Pid, 0); err != syscall.ESRCH {
		t.Fatalf("server was not reaped: %v", err)
	}
	// An orphan may remain briefly as a zombie before init reaps it.
	state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(child)).Output()
	if err == nil && !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
		t.Fatalf("descendant survived group cleanup: %s", state)
	}
}
