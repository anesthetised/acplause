package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellObserverStreamsBeforeExit(t *testing.T) {
	w := newWorkspace(t, Options{OutputLimit: 8})
	call, err := w.prepare("bash", map[string]string{"command": `printf '\342'; while [ ! -f release ]; do sleep 0.01; done; printf '\202\254\000\377xyzMORE' >&2; exit 7`})
	if err != nil {
		t.Fatal(err)
	}
	observer := call.ObserveShell()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- call.Run(ctx) }()
	first := awaitShell(t, observer, func(s ShellSnapshot) bool { return len(s.Output) > 0 })
	if !first.Started || first.Command != call.Command || first.Cwd != w.cwd || !bytes.Equal(first.Output, []byte{0xe2}) || first.Exit != nil {
		t.Fatalf("live: %+v", first)
	}
	select {
	case r := <-done:
		t.Fatalf("exited before release: %+v", r)
	default:
	}
	if err := os.WriteFile(filepath.Join(w.cwd, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if !r.Failed || !strings.Contains(r.Output, "exit status 7") || !strings.Contains(r.Output, "4 more bytes") {
			t.Fatalf("result: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not exit")
	}
	final := observer.Snapshot(0)
	if !bytes.Equal(final.Output, []byte{0xe2, 0x82, 0xac, 0, 0xff, 'x', 'y', 'z'}) || final.Dropped != 4 || final.Exit == nil || final.Exit.Code == nil || *final.Exit.Code != 7 || final.Exit.Signal != "" {
		t.Fatalf("final: %+v", final)
	}
	if tail := observer.Snapshot(5); string(tail.Output) != "xyz" {
		t.Fatalf("tail: %+v", tail)
	}
}

func awaitShell(t *testing.T, o *ShellObserver, ready func(ShellSnapshot) bool) ShellSnapshot {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		s := o.Snapshot(0)
		if ready(s) {
			return s
		}
		select {
		case <-o.Changed():
		case <-timer.C:
			t.Fatalf("shell observation timed out: %+v", s)
		}
	}
}

func TestShellObserverUnreadDoesNotBlock(t *testing.T) {
	w := newWorkspace(t, Options{OutputLimit: 16})
	call, err := w.prepare("bash", map[string]string{"command": `printf '%1000000s' ''; exit 0`})
	if err != nil {
		t.Fatal(err)
	}
	o := call.ObserveShell()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r := call.Run(ctx); r.Failed {
		t.Fatal(r.Output)
	}
	s := o.Snapshot(0)
	if len(s.Output) != 16 || s.Dropped != 1000000-16 || s.Exit == nil || s.Exit.Code == nil || *s.Exit.Code != 0 {
		t.Fatalf("unread observer: %+v", s)
	}
}

func TestShellObserverStartFailure(t *testing.T) {
	w := newWorkspace(t, Options{})
	call, err := w.prepare("bash", map[string]string{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	o := call.ObserveShell()
	if err := os.Remove(w.cwd); err != nil {
		t.Fatal(err)
	}
	if r := call.Run(context.Background()); !r.Failed {
		t.Fatal("missing cwd started")
	}
	if s := o.Snapshot(0); s.Started || s.Exit != nil || len(s.Output) != 0 {
		t.Fatalf("phantom shell: %+v", s)
	}
}

func TestShellObserverSnapshotsAreDetached(t *testing.T) {
	w := newWorkspace(t, Options{})
	c, err := w.prepare("bash", map[string]string{"command": "printf data"})
	if err != nil {
		t.Fatal(err)
	}
	o := c.ObserveShell()
	if c.ObserveShell() != o {
		t.Fatal("observer replaced")
	}
	if r := c.Run(context.Background()); r.Failed {
		t.Fatal(r.Output)
	}
	a := o.Snapshot(0)
	a.Output[0] = 'X'
	*a.Exit.Code = 9
	b := o.Snapshot(0)
	if string(b.Output) != "data" || *b.Exit.Code != 0 {
		t.Fatalf("mutated writer state: %+v", b)
	}
	other, err := w.prepare("list_dir", map[string]string{})
	if err != nil || other.ObserveShell() != nil {
		t.Fatalf("non-shell observation: %v", err)
	}
}

func TestShellObserverCancelledBeforeStart(t *testing.T) {
	w := newWorkspace(t, Options{})
	c, err := w.prepare("bash", map[string]string{"command": "touch ran"})
	if err != nil {
		t.Fatal(err)
	}
	o := c.ObserveShell()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := c.Run(ctx); !r.Failed || !strings.Contains(r.Output, "context canceled") {
		t.Fatalf("result: %+v", r)
	}
	if s := o.Snapshot(0); s.Started || s.Exit != nil {
		t.Fatalf("phantom terminal: %+v", s)
	}
}
