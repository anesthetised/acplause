//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestIssue53SearchRejectsReplacedFIFO(t *testing.T) {
	w := newWorkspace(t, Options{})
	p := w.write("cached.txt", "needle\n")
	entries, err := os.ReadDir(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	root := w.tb.roots.roots[0]
	s := searcher{pattern: []byte("needle"), show: func(name string) string { return name }}
	done := make(chan error, 1)
	go func() { done <- s.visit(context.Background(), w.tb.roots.within(root, "cached.txt"), entries[0]) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if s.matches != 0 {
			t.Fatalf("searched FIFO: %q", s.out.String())
		}
	case <-time.After(time.Second):
		// Old code blocks in Open. Connect a writer, then close it to end ReadAll.
		// O_NONBLOCK prevents cleanup itself from hanging if the reader exited.
		deadline := time.Now().Add(5 * time.Second)
		for {
			writer, err := os.OpenFile(filepath.Clean(p), os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				_ = writer.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("FIFO cleanup: %v", err)
			}
			time.Sleep(time.Millisecond)
		}
		<-done
		t.Fatal("search blocked on a replaced FIFO")
	}
}
