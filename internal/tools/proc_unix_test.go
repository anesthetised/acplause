//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBashTimeoutKillsTheProcessGroup(t *testing.T) {
	w := newWorkspace(t, Options{BashTimeout: 300 * time.Millisecond})
	pidFile := filepath.Join(w.cwd, "child.pid")
	start := time.Now()
	w.fails("bash", map[string]string{"command": "sleep 30 & echo $! > child.pid; wait"}, "timed out after 300ms")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	assertExited(t, pidFile)
}

func TestBashCancellationKillsTheProcessGroup(t *testing.T) {
	w := newWorkspace(t, Options{})
	pidFile := filepath.Join(w.cwd, "child.pid")
	call, err := w.prepare("bash", map[string]string{"command": "sleep 30 & echo $! > child.pid; wait"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !fileExists(pidFile) {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	done := make(chan Result, 1)
	go func() { done <- call.Run(ctx) }()
	select {
	case r := <-done:
		if !r.Failed || !strings.Contains(r.Output, "context canceled") {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop the command")
	}
	assertExited(t, pidFile)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// assertExited checks that the process whose pid is in pidFile is gone.
func assertExited(t *testing.T, pidFile string) {
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
			t.Fatalf("background process %d survived", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
