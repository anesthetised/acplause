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

// TestMCPDetachedStderrHelper keeps stderr open outside the owned process group.
func TestMCPDetachedStderrHelper(t *testing.T) {
	mode := os.Getenv("ACPLAUSE_MCP_DETACHED")
	if mode == "" {
		return
	}
	if mode == "child" {
		_ = os.WriteFile(os.Getenv("DETACHED_PID"), []byte(strconv.Itoa(os.Getpid())), 0600)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	executable, _ := os.Executable()
	cmd := exec.Command(executable, "-test.run=^TestMCPDetachedStderrHelper$")
	cmd.Env = append(os.Environ(), "ACPLAUSE_MCP_DETACHED=child")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = os.Stderr
	if cmd.Start() != nil {
		os.Exit(2)
	}
	for {
		if _, err := os.Stat(os.Getenv("DETACHED_PID")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	os.Exit(0)
}

func TestMCPCloseDoesNotWaitForDetachedStderr(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	executable, _ := os.Executable()
	p := newPeer(context.Background())
	tr, err := startStdio(p, Config{Command: executable, Args: []string{"-test.run=^TestMCPDetachedStderrHelper$"}, Env: map[string]string{"ACPLAUSE_MCP_DETACHED": "parent", "DETACHED_PID": pidFile}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	p.t = tr
	defer tr.Close()
	var child int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(pidFile)
		child, _ = strconv.Atoi(string(raw))
		if child > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if child <= 0 {
		t.Fatal("detached child did not start")
	}
	defer syscall.Kill(child, syscall.SIGKILL)
	closed := make(chan struct{})
	go func() { _ = tr.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("close waited on detached stderr")
	}
}
