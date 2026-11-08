package llm

import (
	"cmp"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func withIdle(c *Client, d time.Duration) *Client {
	c.idleTimeout = d
	return c
}

func TestChatTimesOutOnAStalledStream(t *testing.T) {
	var calls atomic.Int32
	c := withIdle(serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client leaving
		calls.Add(1)
		sse(w, data(`{"choices":[{"delta":{"content":"par"}}]}`))
		<-r.Context().Done()
	}), 50*time.Millisecond)
	start := time.Now()
	_, deltas, err := collect(t, c)
	e, ok := errors.AsType[*Error](err)
	if !ok || e.Kind != KindTimeout {
		t.Fatalf("got %v, want a timeout *Error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s", elapsed)
	}
	if len(deltas) != 1 || calls.Load() != 1 {
		t.Fatalf("deltas=%v calls=%d: a started stream must not be replayed", deltas, calls.Load())
	}
}

func TestChatKeepalivesCountAsActivity(t *testing.T) {
	c := withIdle(serve(t, func(w http.ResponseWriter, _ *http.Request) {
		for range 8 {
			sse(w, ": OPENROUTER PROCESSING\n\n")
			time.Sleep(20 * time.Millisecond)
		}
		sse(w, data(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), data(`[DONE]`))
	}), 100*time.Millisecond)
	comp, _, err := collect(t, c)
	if err != nil || comp.Content != "ok" {
		t.Fatalf("got %+v, %v", comp, err)
	}
}

func TestChatRetriesWhenNoHeadersArrive(t *testing.T) {
	var calls atomic.Int32
	c := withIdle(serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		sse(w, data(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), data(`[DONE]`))
	}), 50*time.Millisecond)
	comp, _, err := collect(t, c)
	if err != nil || comp.Content != "ok" || calls.Load() != 2 {
		t.Fatalf("got %+v, %v after %d calls", comp, err, calls.Load())
	}
}

func TestChatReportsTimeoutWhenHeadersNeverArrive(t *testing.T) {
	var calls atomic.Int32
	c := withIdle(serve(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		<-r.Context().Done()
	}), 20*time.Millisecond)
	_, _, err := collect(t, c)
	if e, ok := errors.AsType[*Error](err); !ok || e.Kind != KindTimeout || calls.Load() != 4 {
		t.Fatalf("got %v after %d calls, want a timeout after 4", err, calls.Load())
	}
}

func TestModelsTimeOutWhileDecoding(t *testing.T) {
	c := withIdle(serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), 50*time.Millisecond)
	_, err := c.Models(context.Background())
	if e, ok := errors.AsType[*Error](err); !ok || e.Kind != KindTimeout {
		t.Fatalf("got %v, want a timeout *Error", err)
	}
}

func TestIdleTimeoutConfig(t *testing.T) {
	if c := New(Config{}); c.idleTimeout != DefaultIdleTimeout {
		t.Fatalf("default: %s", c.idleTimeout)
	}
	for _, d := range []time.Duration{3 * time.Second, 0} {
		if c := New(Config{IdleTimeout: &d}); c.idleTimeout != cmp.Or(d, math.MaxInt64) {
			t.Errorf("%s: got %s", d, c.idleTimeout)
		}
	}
}
