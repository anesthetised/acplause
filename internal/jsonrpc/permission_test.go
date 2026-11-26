package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
)

func permissionCall(ctx context.Context, c *Conn, method string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.CallPermission(ctx, method, nil, &struct{}{}) }()
	return done
}

func TestPermissionCancelWithFullOrdinaryQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, out, release := heldConn(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first := permissionCall(ctx, c, "first")
		synctest.Wait() // writer holds first
		early, cancelEarly := context.WithCancel(t.Context())
		second := permissionCall(early, c, "second")
		synctest.Wait()
		cancelEarly()
		expectCallError(t, second, context.Canceled)
		for i := 1; i < 256; i++ {
			if err := c.Notify("filler", i); err != nil {
				t.Fatal(err)
			}
		}
		blockedCtx, abandon := context.WithCancel(t.Context())
		blocked := permissionCall(blockedCtx, c, "unsent")
		synctest.Wait()
		if got := len(c.cancels.slots); got != 3 {
			t.Fatalf("expected slots for first, queued cancel and unsent permission, got %d", got)
		}
		abandon()
		expectCallError(t, blocked, context.Canceled)
		cancel()
		expectCallError(t, first, context.Canceled)
		c.deliver(&inbound{ID: []byte("1"), Result: []byte("{}")})
		if len(c.calls) != 0 || len(c.cancels.slots) != 2 {
			t.Fatal("late response or unsent permission changed queued reservations")
		}
		closed := make(chan error, 1)
		go func() { closed <- c.Close() }()
		synctest.Wait()
		release()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 259 {
			t.Fatalf("got %d lines", len(lines))
		}
		assertJSON(t, lines[0], `{"jsonrpc":"2.0","id":1,"method":"first"}`)
		assertJSON(t, lines[1], `{"jsonrpc":"2.0","id":2,"method":"second"}`)
		assertJSON(t, lines[2], `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":2}}`)
		for i := 1; i < 256; i++ {
			assertJSON(t, lines[i+2], fmt.Sprintf(`{"jsonrpc":"2.0","method":"filler","params":%d}`, i))
		}
		assertJSON(t, lines[258], `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`)
	})
}

// The reader has handled session/cancel and is delivering the very next line
// before the permission worker acquires the connection lock.
func TestPermissionCancelledBeforeResponseDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, out, release := heldConn(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := permissionCall(ctx, c, "ask")
		synctest.Wait()
		answer := &inbound{ID: []byte("1"), Result: []byte("{}")}
		c.mu.Lock()
		cancel()
		c.completeLocked("1", c.calls["1"], answer)
		c.mu.Unlock()
		c.deliver(answer) // a duplicate response cannot consume the slot again
		expectCallError(t, done, context.Canceled)
		release()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 || len(c.cancels.slots) != 0 {
			t.Fatalf("wire lines %q, remaining reservations %d", lines, len(c.cancels.slots))
		}
		assertJSON(t, lines[1], `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`)
	})
}

func TestPermissionReservationLimitAndResponseWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, out, release := heldConn(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		firstCtx, cancelFirst := context.WithCancel(t.Context())
		defer cancelFirst()
		first := permissionCall(firstCtx, c, "first")
		synctest.Wait()
		var pending []<-chan error
		for range 255 {
			pending = append(pending, permissionCall(ctx, c, "ask"))
		}
		synctest.Wait()
		pending = append(pending, permissionCall(ctx, c, "next"))
		synctest.Wait()
		if len(c.calls) != 256 || len(c.ordinary.lines) != 255 || len(c.cancels.slots) != 256 {
			t.Fatal("257th permission did not wait for a reservation before ordinary admission")
		}
		c.deliver(&inbound{ID: []byte("1"), Result: []byte("{}")})
		cancelFirst() // the delivered response owns this reservation
		synctest.Wait()
		if len(c.calls) != 256 || len(c.ordinary.lines) != 256 {
			t.Fatal("delivering the first answer did not admit the next permission")
		}
		cancel()
		expectCallError(t, first, nil)
		for _, done := range pending {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("pending permission: %v", err)
			}
		}
		if len(c.cancels.lines) != 256 || len(c.calls) != 0 {
			t.Fatal("pending permissions did not fill the separate cancellation FIFO")
		}
		blockedCtx, abandon := context.WithCancel(t.Context())
		blocked := permissionCall(blockedCtx, c, "unsent")
		synctest.Wait()
		abandon()
		expectCallError(t, blocked, context.Canceled)
		release()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		// All 257 requests precede every cancel. The answered ID is excluded
		// and each remaining request has exactly one matching cancellation.
		ids := map[ID]bool{}
		lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
		if len(lines) != 513 {
			t.Fatalf("got %d lines", len(lines))
		}
		for i, line := range lines {
			var msg struct {
				ID     ID
				Method string
				Params struct{ RequestID ID }
			}
			if err := json.Unmarshal(line, &msg); err != nil {
				t.Fatal(err)
			}
			if i < 257 {
				if msg.Method == CancelRequestMethod || msg.Method == "unsent" || msg.ID == "" || ids[msg.ID] {
					t.Fatalf("unexpected request: %s", line)
				}
				ids[msg.ID] = true
			} else {
				id := msg.Params.RequestID
				if msg.Method != CancelRequestMethod || !ids[id] || id == "1" {
					t.Fatalf("unexpected cancellation: %s", line)
				}
				delete(ids, id)
			}
		}
		if len(c.cancels.slots) != 0 || len(c.ordinary.slots) != 0 {
			t.Fatal("writer retained slots after draining both FIFOs")
		}
	})
}

type stoppedWriter struct {
	release chan struct{}
	fail    bool
}

func (w stoppedWriter) Write(b []byte) (int, error) {
	<-w.release
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

func TestPermissionWaitersWakeOnShutdown(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "write failure"}[fail], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				out := stoppedWriter{make(chan struct{}), fail}
				c := NewConn(nil, out, slog.New(slog.DiscardHandler))
				if err := c.Notify("hold_writer", nil); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				var calls []<-chan error
				// Leave a cancellation line for the writer to drain or discard.
				ctx, cancel := context.WithCancel(t.Context())
				abandoned := permissionCall(ctx, c, "abandoned")
				synctest.Wait()
				cancel()
				expectCallError(t, abandoned, context.Canceled)
				for range 254 {
					calls = append(calls, permissionCall(t.Context(), c, "admitted"))
				}
				synctest.Wait()
				if err := c.Notify("last_ordinary_slot", nil); err != nil {
					t.Fatal(err)
				}
				calls = append(calls, permissionCall(t.Context(), c, "waiting_for_ordinary"))
				synctest.Wait()
				calls = append(calls, permissionCall(t.Context(), c, "waiting_for_reservation"))
				synctest.Wait()
				if len(c.calls) != 254 || len(c.cancels.slots) != 256 || len(c.cancels.lines) != 1 {
					t.Fatal("missing admission or response waiter")
				}
				if fail {
					close(out.release)
				} else {
					go func() { _ = c.Close() }()
				}
				for _, done := range calls {
					if err := <-done; !errors.Is(err, ErrClosed) {
						t.Fatalf("shutdown: %v", err)
					}
				}
				if !fail {
					close(out.release)
				}
				err := c.Close()
				if (fail && !errors.Is(err, io.ErrClosedPipe)) || (!fail && err != nil) {
					t.Fatalf("close: %v", err)
				}
				if len(c.cancels.slots) != 0 || len(c.ordinary.slots) != 0 || len(c.calls) != 0 {
					t.Fatal("shutdown retained slots or calls")
				}
			})
		})
	}
}

func TestWriterFailureWakesIdleReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, peer := io.Pipe()
		defer peer.Close()
		output := stoppedWriter{release: make(chan struct{}), fail: true}
		c := NewConn(in, output, slog.New(slog.DiscardHandler))
		served := make(chan error, 1)
		go func() { served <- c.Serve(t.Context(), nil) }()
		synctest.Wait() // Serve's line reader is inside Read on a quiet pipe.
		if err := c.Notify("fail", nil); err != nil {
			t.Fatal(err)
		}
		close(output.release)
		expectCallError(t, served, ErrClosed)
		// With the reader stopped, the still-open peer cannot write another
		// byte. A successful write would mean a stranded read consumed it.
		if _, err := peer.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("input survived transport failure: %v", err)
		}
		if err := c.Close(); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("close lost the write failure: %v", err)
		}
	})
}
