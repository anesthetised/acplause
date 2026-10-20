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
	w := newWorkspace(t, Options{BashTimeout: new(300 * time.Millisecond)})
	pidFile := filepath.Join(w.cwd, "child.pid")
	start := time.Now()
	w.fails("bash", map[string]string{"command": "sleep 30 & echo $! > child.pid; wait"}, "timed out after 300ms")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	assertExited(t, pidFile)
}

func TestBashCancellationKillsTheProcessGroup(t *testing.T) {
	w := newWorkspace(t, Options{BashTimeout: new(time.Duration(0))})
	pidFile := filepath.Join(w.cwd, "child.pid")
	// Publish only a complete PID: cancellation must not interrupt its write.
	call, err := w.prepare("bash", map[string]string{"command": "sleep 30 & echo $! > child.pid.tmp && mv child.pid.tmp child.pid; wait"})
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
func TestBashSuccessKillsBackgroundChildren(t *testing.T) {
	for name, child := range map[string]string{
		"holding output": "sleep 30 &",
		"detached":       "sleep 30 >/dev/null 2>&1 &",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorkspace(t, Options{})
			r := w.run("bash", map[string]string{"command": child + " echo $! > child.pid; echo started"})
			if r.Failed || r.Output != "started\n" {
				t.Fatalf("exit 0 reported as %+v", r)
			}
			assertExited(t, filepath.Join(w.cwd, "child.pid"))
		})
	}
}

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

func TestShellObserverSignalsAndCancellation(t *testing.T) {
	for _, mode := range []string{"signal", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			opts := Options{BashTimeout: new(time.Duration(0))}
			command := "printf ready; sleep 60"
			if mode == "timeout" {
				opts.BashTimeout = new(100 * time.Millisecond)
			}
			if mode == "signal" {
				command = "kill -TERM $$"
			}
			w := newWorkspace(t, opts)
			call, err := w.prepare("bash", map[string]string{"command": command})
			if err != nil {
				t.Fatal(err)
			}
			o := call.ObserveShell()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Result, 1)
			go func() { done <- call.Run(ctx) }()
			if mode == "cancel" {
				awaitShell(t, o, func(s ShellSnapshot) bool { return string(s.Output) == "ready" })
				cancel()
			}
			select {
			case r := <-done:
				if !r.Failed {
					t.Fatalf("succeeded: %+v", r)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shell survived")
			}
			s := o.Snapshot(0)
			want := "SIGKILL"
			if mode == "signal" {
				want = "SIGTERM"
			}
			if s.Exit == nil || s.Exit.Code != nil || s.Exit.Signal != want {
				t.Fatalf("status: %+v", s.Exit)
			}
		})
	}
	if status := shellExit(nil); status.Code != nil || status.Signal != "" {
		t.Fatalf("unknown completion: %+v", status)
	}
}

func TestShellObserverPreservesWaitDelay(t *testing.T) {
	w := newWorkspace(t, Options{})
	call, err := w.prepare("bash", map[string]string{"command": "echo $$ > shell.pid; sleep 60 & printf ready; exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	o := call.ObserveShell()
	done := make(chan Result, 1)
	go func() { done <- call.Run(context.Background()) }()
	awaitShell(t, o, func(s ShellSnapshot) bool { return len(s.Output) > 0 })
	raw, err := os.ReadFile(filepath.Join(w.cwd, "shell.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid: %q, %v", raw, err)
	}
	defer syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case r := <-done:
		if r.Failed || r.Output != "ready" {
			t.Fatalf("pipe wait result: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observer defeated WaitDelay")
	}
	s := o.Snapshot(0)
	if s.Exit == nil || s.Exit.Code == nil || *s.Exit.Code != 0 || string(s.Output) != "ready" {
		t.Fatalf("shell status: %+v", s)
	}
}
