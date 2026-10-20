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

func TestEditBatchRejectsFIFOAtPreviewAndApproval(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "approved"}[pending], func(t *testing.T) {
			w := newWorkspace(t, Options{})
			path := w.write("a", "one")
			c, err := w.prepare("edit_batch", batchArgs("a", "one", "new"))
			if err != nil {
				t.Fatal(err)
			}
			if pending {
				if _, err := c.Preview(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			keeper, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer keeper.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan string, 1)
			go func() {
				if pending {
					done <- c.Run(ctx).Output
				} else {
					_, err := c.Preview(ctx)
					if err == nil {
						done <- "unexpected successful preview"
					} else {
						done <- err.Error()
					}
				}
			}()
			select {
			case result := <-done:
				if !strings.Contains(result, "regular file") {
					t.Fatalf("FIFO accepted: %s", result)
				}
			case <-time.After(time.Second):
				cancel()
				_ = keeper.Close()
				<-done
				t.Fatal("batch edit blocked on FIFO")
			}
			if info, err := os.Lstat(filepath.Join(w.cwd, "a")); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatalf("FIFO modified: %v", err)
			}
		})
	}
}
