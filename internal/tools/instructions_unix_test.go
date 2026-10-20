//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveInstructionsRejectsFIFO(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("AGENTS.md", "Root.")
	w.write("child/keep", "not instructions")
	pipe := filepath.Join(w.cwd, "child/AGENTS.md")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep a writer alive so a reader missing the regular-file check would block.
	keeper, err := os.OpenFile(pipe, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		sources []Instruction
		err     error
	}
	done := make(chan result, 1)
	go func() {
		sources, err := w.tb.ResolveInstructions(ctx, "child")
		done <- result{sources, err}
	}()
	select {
	case got := <-done:
		if got.err == nil || len(got.sources) != 0 || !strings.Contains(got.err.Error(), pipe) || !strings.Contains(got.err.Error(), "regular file") {
			t.Fatalf("FIFO returned partial rules: %#v, %v", got.sources, got.err)
		}
	case <-time.After(time.Second):
		cancel()
		_ = keeper.Close()
		<-done
		t.Fatal("instruction lookup blocked on a FIFO")
	}
}

func TestReadInstructionRejectsDevice(t *testing.T) {
	// Exercise the same bounded source reader on an existing device, without
	// requiring mknod privileges or creating files in /dev.
	root, err := os.OpenRoot("/dev")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	source := target{root: root, rel: "null", abs: "/dev/null"}
	got, err := readInstruction(context.Background(), source)
	if err == nil || got != nil || !strings.Contains(err.Error(), "/dev/null") || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("device source: %#v, %v", got, err)
	}
}
