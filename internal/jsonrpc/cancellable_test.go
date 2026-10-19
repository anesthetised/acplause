package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// heldOutput keeps the single writer blocked until the test releases it.
type heldOutput struct {
	release chan struct{}
	bytes.Buffer
}

func (w *heldOutput) Write(b []byte) (int, error) {
	<-w.release
	return w.Buffer.Write(b)
}

func heldConn(t *testing.T) (*Conn, *heldOutput, func()) {
	t.Helper()
	out := &heldOutput{release: make(chan struct{})}
	c := NewConn(nil, out, slog.New(slog.DiscardHandler))
	release := sync.OnceFunc(func() { close(out.release) })
	t.Cleanup(func() {
		release()
		_ = c.Close()
	})
	return c, out, release
}

func expectCallError(t *testing.T, errs <-chan error, want error) {
	t.Helper()
	synctest.Wait()
	select {
	case err := <-errs:
		if !errors.Is(err, want) {
			t.Fatalf("call: got %v, want %v", err, want)
		}
	default:
		t.Fatal("call is still blocked")
	}
}

func TestCallCancellableOrdersCancellationAndDropsLateResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, out, release := heldConn(t)
		ctx, cancel := context.WithCancel(context.Background())
		errs := make(chan error, 1)
		go func() { errs <- c.CallCancellable(ctx, "ask", nil, &struct{}{}) }()
		synctest.Wait() // the request is enqueued and the writer is blocked
		cancel()
		expectCallError(t, errs, context.Canceled)
		c.deliver(&inbound{ID: []byte(`1`), Result: []byte(`{}`)})
		if len(c.calls) != 0 {
			t.Fatal("cancelled waiter retained")
		}
		if err := c.Notify("still_alive", nil); err != nil {
			t.Fatal(err)
		}
		release()
		_ = c.Close()
		lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
		if len(lines) != 3 {
			t.Fatalf("output: %s", out.Bytes())
		}
		assertJSON(t, string(lines[0]), `{"jsonrpc":"2.0","id":1,"method":"ask"}`)
		assertJSON(t, string(lines[1]), `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`)
		assertJSON(t, string(lines[2]), `{"jsonrpc":"2.0","method":"still_alive"}`)
	})
}

func TestCallCancellableBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, out, release := heldConn(t)
		ctx, cancel := context.WithCancel(context.Background())
		sent := make(chan error, 1)
		go func() { sent <- c.CallCancellable(ctx, "sent", nil, &struct{}{}) }()
		synctest.Wait()
		for range cap(c.queue) {
			if err := c.Notify("filler", nil); err != nil {
				t.Fatal(err)
			}
		}
		unsent := make(chan error, 1)
		go func() { unsent <- c.CallCancellable(ctx, "unsent", nil, &struct{}{}) }()
		synctest.Wait() // the second call is waiting for queue space
		cancel()
		expectCallError(t, sent, context.Canceled)
		expectCallError(t, unsent, context.Canceled)
		if len(c.calls) != 0 {
			t.Fatal("cancelled waiters retained")
		}
		release()
		_ = c.Close()
		if bytes.Contains(out.Bytes(), []byte("cancel_request")) || bytes.Contains(out.Bytes(), []byte("unsent")) {
			t.Fatalf("unexpected request/cancellation: %s", out.Bytes())
		}
		if got := bytes.Count(out.Bytes(), []byte("\n")); got != cap(c.queue)+1 {
			t.Fatalf("wrote %d messages", got)
		}
	})
}

func TestCallCancellableResponseWins(t *testing.T) {
	for _, tt := range []struct {
		name, result, rpcError, wantError string
	}{
		{name: "success", result: `{"n":7}`},
		{name: "peer error", rpcError: `{"code":-32603,"message":"no"}`, wantError: "jsonrpc error -32603: no"},
		{name: "bad result", result: `[]`, wantError: "decode result of ask"},
		{name: "bad error", rpcError: `[]`, wantError: "decode error of ask"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, out, release := heldConn(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var result struct{ N int }
				errs := make(chan error, 1)
				go func() { errs <- c.CallCancellable(ctx, "ask", nil, &result) }()
				synctest.Wait()
				msg := &inbound{ID: []byte(`1`), Result: []byte(tt.result)}
				if tt.rpcError != "" {
					msg.Error = []byte(tt.rpcError)
				}
				c.deliver(msg)
				cancel() // response completion precedes cancellation, even if both are ready
				synctest.Wait()
				err := <-errs
				if tt.wantError == "" {
					if err != nil || result.N != 7 {
						t.Fatalf("result: %+v, error: %v", result, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error: %v, want %s", err, tt.wantError)
				}
				release()
				_ = c.Close()
				if bytes.Contains(out.Bytes(), []byte("cancel_request")) {
					t.Fatalf("completed request cancelled: %s", out.Bytes())
				}
			})
		})
	}
}

func TestCallCancellableCloseUnderBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, out, release := heldConn(t)
		errs := make(chan error, 2)
		go func() { errs <- c.CallCancellable(context.Background(), "sent", nil, &struct{}{}) }()
		synctest.Wait()
		for range cap(c.queue) {
			_ = c.Notify("filler", nil)
		}
		go func() { errs <- c.CallCancellable(context.Background(), "unsent", nil, &struct{}{}) }()
		synctest.Wait()
		closed := make(chan struct{})
		go func() { _ = c.Close(); close(closed) }()
		expectCallError(t, errs, ErrClosed)
		expectCallError(t, errs, ErrClosed)
		c.deliver(&inbound{ID: []byte(`1`), Result: []byte(`{}`)})
		if len(c.calls) != 0 {
			t.Fatal("closed waiters retained")
		}
		release()
		<-closed
		if bytes.Contains(out.Bytes(), []byte("cancel_request")) || bytes.Contains(out.Bytes(), []byte("unsent")) {
			t.Fatalf("unexpected request/cancellation after close: %s", out.Bytes())
		}
	})
}

func TestCallCancellableNotEnqueued(t *testing.T) {
	for _, reason := range []string{"cancelled", "closed", "invalid params"} {
		t.Run(reason, func(t *testing.T) {
			c, out, release := heldConn(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var params any
			switch reason {
			case "cancelled":
				cancel()
			case "closed":
				release()
				_ = c.Close()
			case "invalid params":
				params = make(chan int)
			}
			err := c.CallCancellable(ctx, "ask", params, &struct{}{})
			if err == nil || (reason == "cancelled" && !errors.Is(err, context.Canceled)) || (reason == "closed" && !errors.Is(err, ErrClosed)) {
				t.Fatalf("call: %v", err)
			}
			release()
			_ = c.Close()
			if len(c.calls) != 0 || out.Len() != 0 {
				t.Fatalf("unsent request retained or written: %s", out.Bytes())
			}
		})
	}
}

func TestOrdinaryCallKeepsEnqueueAndPermissionSemantics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, out, release := heldConn(t)
		_ = c.Notify("block_writer", nil)
		synctest.Wait()
		for range cap(c.queue) {
			_ = c.Notify("filler", nil)
		}
		ctx, cancel := context.WithCancel(context.Background())
		errs := make(chan error, 1)
		go func() { errs <- c.Call(ctx, "session/request_permission", nil, &struct{}{}) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-errs:
			t.Fatalf("ordinary Call abandoned its enqueue: %v", err)
		default:
		}
		release()
		expectCallError(t, errs, context.Canceled)
		_ = c.Close()
		if !bytes.Contains(out.Bytes(), []byte("session/request_permission")) || bytes.Contains(out.Bytes(), []byte("cancel_request")) {
			t.Fatalf("ordinary Call changed wire behavior: %s", out.Bytes())
		}
	})
}

func TestCallCancellableConcurrentRequests(t *testing.T) {
	p := newPeer(t, Methods{})
	const count = 12
	cancels := make([]context.CancelFunc, count)
	errs := make([]chan error, count)
	for i := range count {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancels[i] = cancel
		errs[i] = make(chan error, 1)
		go func() { errs[i] <- p.conn.CallCancellable(ctx, fmt.Sprintf("ask_%d", i), nil, &struct{}{}) }()
	}
	ids := make(map[ID]int)
	for range count {
		var req struct {
			ID     ID
			Method string
		}
		if err := json.Unmarshal([]byte(p.recv()), &req); err != nil {
			t.Fatal(err)
		}
		var i int
		if _, err := fmt.Sscanf(req.Method, "ask_%d", &i); err != nil {
			t.Fatal(err)
		}
		ids[req.ID] = i
	}
	for id, i := range ids {
		if i%2 == 0 {
			cancels[i]()
		} else {
			p.send(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{}}`)
		}
	}
	for range count / 2 {
		var n struct {
			Method string
			Params struct{ RequestID ID }
		}
		if err := json.Unmarshal([]byte(p.recv()), &n); err != nil {
			t.Fatal(err)
		}
		i, ok := ids[n.Params.RequestID]
		if n.Method != CancelRequestMethod || !ok || i%2 != 0 {
			t.Fatalf("wrong cancellation: %+v", n)
		}
		delete(ids, n.Params.RequestID)
		p.send(`{"jsonrpc":"2.0","id":` + string(n.Params.RequestID) + `,"result":{}}`)
	}
	for i := range count {
		if err := <-errs[i]; (i%2 == 0 && !errors.Is(err, context.Canceled)) || (i%2 != 0 && err != nil) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}
