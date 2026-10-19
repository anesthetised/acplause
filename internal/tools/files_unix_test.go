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

func TestFileToolsRejectFIFO(t *testing.T) {
	for _, name := range []string{"read_file", "write_file", "edit"} {
		t.Run(name, func(t *testing.T) {
			w := newWorkspace(t, Options{})
			p := filepath.Join(w.cwd, "pipe")
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
			// Keep both ends open so a broken implementation waits for data.
			keeper, err := os.OpenFile(p, os.O_RDWR|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer keeper.Close()
			call, err := w.prepare(name, map[string]string{"path": "pipe", "content": "x", "old_text": "x", "new_text": "y"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Result, 1)
			go func() { done <- call.Run(ctx) }()
			select {
			case result := <-done:
				if !result.Failed || !strings.Contains(result.Output, "regular file") {
					t.Fatalf("FIFO was not rejected: %+v", result)
				}
			case <-time.After(time.Second):
				cancel()
				_ = keeper.Close()
				<-done
				t.Fatal("file tool blocked on a FIFO")
			}
		})
	}
}
