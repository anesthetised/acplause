package mcp

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type cleanupLog struct {
	mu      sync.Mutex
	data    bytes.Buffer
	written chan struct{}
}

func (l *cleanupLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	n, err := l.data.Write(p)
	l.mu.Unlock()
	select {
	case l.written <- struct{}{}:
	default:
	}
	return n, err
}

func (l *cleanupLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.String()
}

func TestStdioCleanupFailureReportedOnce(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "background"}[background], func(t *testing.T) {
			capture := &cleanupLog{written: make(chan struct{}, 4)}
			log := slog.New(slog.NewJSONHandler(capture, nil))
			cfg := helperConfig(t, "cleanup")
			cfg.Env["PRIVATE_TOKEN"] = "configuration-secret"
			s, err := connectServer(t.Context(), t.Context(), cfg, t.TempDir(), log)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			tr := s.peer.t.(*stdioTransport)
			original := tr.process.close
			sentinel := errors.New("error-text-secret")
			var closes atomic.Int32
			tr.process.close = func() error {
				closes.Add(1)
				return errors.Join(original(), sentinel)
			}
			if background {
				s.peer.fail(errors.New("peer failure"))
				select {
				case <-capture.written:
				case <-time.After(5 * time.Second):
					t.Fatal("background cleanup failure was not logged")
				}
			} else {
				s.close()
			}
			if err := tr.Close(); !errors.Is(err, sentinel) {
				t.Fatalf("cleanup error lost: %v", err)
			}
			s.close()
			text := capture.text()
			if closes.Load() != 1 || strings.Count(text, "MCP stdio cleanup failed") != 1 || strings.Count(text, "\n") != 1 {
				t.Fatalf("cleanup should warn exactly once: closes=%d log=%q", closes.Load(), text)
			}
			if strings.Contains(text, "configuration-secret") || strings.Contains(text, "error-text-secret") {
				t.Fatalf("cleanup log exposed private text: %s", text)
			}
		})
	}
}

func TestStdioCleanupSuccessIsQuiet(t *testing.T) {
	capture := &cleanupLog{written: make(chan struct{}, 1)}
	s, err := connectServer(t.Context(), t.Context(), helperConfig(t, "cleanup"), t.TempDir(), slog.New(slog.NewJSONHandler(capture, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	if text := capture.text(); text != "" {
		t.Fatalf("successful cleanup logged a failure: %q", text)
	}
}
