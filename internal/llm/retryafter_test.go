package llm

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordWaits replaces the client's retry timer with one that fires at once
// and records the requested delays.
func recordWaits(c *Client) *[]time.Duration {
	var mu sync.Mutex
	var waits []time.Duration
	c.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	return &waits
}

func retryAfterServer(t *testing.T, header string, status int) (*Client, *atomic.Int32) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			http.Error(w, "slow down", status)
			return
		}
		sse(w, data(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), data(`[DONE]`))
	})
	return c, &calls
}

func TestRetryAfterReplacesBackoff(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		c, calls := retryAfterServer(t, "2", status)
		waits := recordWaits(c)
		comp, _, err := collect(t, c)
		if err != nil || comp.Content != "ok" || calls.Load() != 2 {
			t.Fatalf("%d: got %+v, %v after %d calls", status, comp, err, calls.Load())
		}
		if len(*waits) != 1 || (*waits)[0] != 2*time.Second {
			t.Fatalf("%d: waits %v, want [2s]", status, *waits)
		}
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	c, _ := retryAfterServer(t, time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat), http.StatusTooManyRequests)
	waits := recordWaits(c)
	if _, _, err := collect(t, c); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 1 || (*waits)[0] < 5*time.Second || (*waits)[0] > 10*time.Second {
		t.Fatalf("waits %v, want about 10s", *waits)
	}
}

func TestRetryAfterIsCapped(t *testing.T) {
	c, _ := retryAfterServer(t, "86400", http.StatusTooManyRequests)
	waits := recordWaits(c)
	if _, _, err := collect(t, c); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 1 || (*waits)[0] != maxRetryAfter {
		t.Fatalf("waits %v, want [%s]", *waits, maxRetryAfter)
	}
}

func TestMissingOrMalformedRetryAfterKeepsBackoff(t *testing.T) {
	for _, header := range []string{"", "soon", "-3"} {
		c, _ := retryAfterServer(t, header, http.StatusTooManyRequests)
		waits := recordWaits(c)
		if _, _, err := collect(t, c); err != nil {
			t.Fatal(err)
		}
		if len(*waits) != 1 || (*waits)[0] != c.retryDelay {
			t.Fatalf("%q: waits %v, want [%s]", header, *waits, c.retryDelay)
		}
	}
}

func TestRetryAfterWaitHonoursCancellation(t *testing.T) {
	c, calls := retryAfterServer(t, "30", http.StatusTooManyRequests)
	ctx, cancel := context.WithCancel(context.Background())
	c.after = func(time.Duration) <-chan time.Time {
		cancel() // cancelled during the wait; the timer never fires
		return nil
	}
	_, err := c.Chat(ctx, Request{Model: "m"}, func(Delta) {})
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("got %v after %d calls, want context.Canceled after 1", err, calls.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":                              0,
		"0":                             0,
		"7":                             7 * time.Second,
		"Sun, 08 Nov 2026 12:00:09 GMT": 9 * time.Second,
		"Sun, 08 Nov 2026 11:59:00 GMT": 0,
		"99999999999999999999":          0,
		"1.5":                           0,
	} {
		if got := parseRetryAfter(value, now); got != want {
			t.Errorf("%q: got %s, want %s", value, got, want)
		}
	}
}
