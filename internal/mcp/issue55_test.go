package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type issue55RoundTripper func(*http.Request) (*http.Response, error)

func (f issue55RoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func issue55Tool(p *peer) Tool {
	ctx, cancel := context.WithCancel(p.ctx)
	s := &server{peer: p}
	tool := Tool{OriginalName: "effect", ctx: ctx, cancel: cancel, server: s}
	s.tools = []Tool{tool}
	return tool
}

// issue55Revoke models the definition publication critical section. Real
// tools/list refresh and notification publication are covered separately.
func issue55Revoke(tool Tool) {
	tool.server.mu.Lock()
	tool.cancel()
	tool.server.tools = nil
	tool.server.mu.Unlock()
}

func issue55Until(t *testing.T, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("dispatch never reached queue")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestIssue55QueuedToolRevokedBeforeAdmission(t *testing.T) {
	for _, transport := range []string{"http", "stdio"} {
		t.Run(transport, func(t *testing.T) {
			p := newPeer(t.Context())
			var unblock func()
			var dispatched func() bool
			if transport == "http" {
				var calls atomic.Int32
				tr := newHTTP(p, Config{URL: "http://fixture.invalid"})
				tr.client.Transport = issue55RoundTripper(func(r *http.Request) (*http.Response, error) {
					var req wire
					_ = json.NewDecoder(r.Body).Decode(&req)
					if req.Method == "tools/call" {
						calls.Add(1)
					}
					return nil, io.EOF
				})
				tr.slots = make(chan struct{}, 1)
				tr.slots <- struct{}{}
				unblock = func() { <-tr.slots }
				dispatched = func() bool { return calls.Load() != 0 }
				p.t = tr
			} else {
				trace := filepath.Join(t.TempDir(), "calls")
				cfg := helperConfig(t, "queued")
				cfg.Env["ACPLAUSE_MCP_TRACE"] = trace
				tr, err := startStdio(p, cfg, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				p.t = tr
				unblock = tr.start
				dispatched = func() bool {
					b, _ := os.ReadFile(trace)
					return strings.Contains(string(b), `"method":"tools/call"`)
				}
			}
			defer p.t.Close()
			tool := issue55Tool(p)
			done := make(chan error, 1)
			go func() { _, err := tool.Call(t.Context(), `{}`); done <- err }()
			issue55Until(t, func() bool {
				if tr, ok := p.t.(*stdioTransport); ok {
					return len(tr.writes) == 1
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				return len(p.calls) == 1
			})
			issue55Revoke(tool)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("revoked queued call succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("revocation waited for transport capacity")
			}
			unblock()
			// A ping fences the writer/job after capacity becomes available.
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			var result any
			_ = p.call(ctx, "ping", map[string]any{}, &result)
			if dispatched() {
				t.Fatal("revoked queued tools/call reached transport I/O")
			}
		})
	}
}

func TestIssue55HTTPRefreshWinsAdmission(t *testing.T) {
	p := newPeer(t.Context())
	tr := newHTTP(p, Config{URL: "http://fixture.invalid"})
	p.t = tr
	defer tr.Close()
	var calls atomic.Int32
	tr.client.Transport = issue55RoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.EOF
	})
	tool := issue55Tool(p)
	done := make(chan error, 1)
	func() {
		tool.server.mu.Lock()
		defer tool.server.mu.Unlock()
		go func() { _, err := tool.Call(t.Context(), `{}`); done <- err }()
		// Capacity is acquired, but admission cannot pass the publication lock.
		issue55Until(t, func() bool { return len(tr.slots) == 1 })
		tool.cancel()
		tool.server.tools = nil
	}()
	select {
	case err := <-done:
		if err == nil || calls.Load() != 0 {
			t.Fatalf("refresh lost admission: err=%v HTTP calls=%d", err, calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("revoked admission never settled")
	}
}

func TestIssue55StdioAdmissionSurvivesRefreshBeforeWrite(t *testing.T) {
	p := newPeer(t.Context())
	tr, err := startStdio(p, helperConfig(t, "admitted"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.t = tr
	defer tr.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tool := issue55Tool(p)
	done := make(chan error, 1)
	go func() {
		result, err := tool.Call(ctx, `{"receipt":"original-effect"}`)
		if err == nil && string(result.StructuredContent) != `{"receipt":"original-effect"}` {
			err = io.ErrUnexpectedEOF
		}
		done <- err
	}()
	issue55Until(t, func() bool { return len(tr.writes) == 1 })
	item := <-tr.writes
	admit := item.dispatch.admit
	entered, release := make(chan struct{}), make(chan struct{}, 1)
	defer close(release)
	item.dispatch.admit = func() error {
		if err := admit(); err != nil {
			return err
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	tr.writes <- item
	tr.start()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("writer never admitted call")
	}
	// Refresh and snapshots must remain available after admission, even when
	// the selected writer has not yet performed its pipe I/O.
	if !tool.server.mu.TryLock() {
		t.Fatal("admission retained catalog lock")
	}
	tool.cancel()
	tool.server.tools = nil
	tool.server.mu.Unlock()
	select {
	case err := <-done:
		t.Fatalf("refresh abandoned admitted write: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("admitted write never returned its result")
	}
}

func TestIssue55AdmittedCallRetainsCancellation(t *testing.T) {
	for _, reason := range []string{"turn", "timeout", "peer"} {
		t.Run(reason, func(t *testing.T) {
			p := newPeer(t.Context())
			tr := newHTTP(p, Config{URL: "http://fixture.invalid"})
			p.t = tr
			defer tr.Close()
			tool := issue55Tool(p)
			entered := make(chan struct{})
			var calls atomic.Int32
			tr.client.Transport = issue55RoundTripper(func(r *http.Request) (*http.Response, error) {
				var req wire
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Method != "tools/call" {
					return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
				}
				calls.Add(1)
				close(entered)
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if reason == "timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Second)
				defer stop()
			}
			done := make(chan error, 1)
			go func() { _, err := tool.Call(ctx, `{}`); done <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("HTTP call never admitted")
			}
			issue55Revoke(tool)
			select {
			case err := <-done:
				t.Fatalf("refresh cancelled admitted call: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			switch reason {
			case "turn":
				cancel()
			case "peer":
				p.fail(errors.New("peer replaced"))
			}
			select {
			case err := <-done:
				if err == nil || calls.Load() != 1 || (reason == "timeout" && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("cancellation=%s err=%v tools/call count=%d", reason, err, calls.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("admission detached caller/peer cancellation")
			}
		})
	}
}

// Scripted transports have no queue; existing peer tests dispatch immediately.
func (t scriptedTransport) sendRequest(ctx context.Context, b []byte, dispatch *dispatch) error {
	if dispatch != nil {
		if err := dispatch.admit(); err != nil {
			return err
		}
	}
	return t.Send(ctx, b)
}

func TestIssue55MCPDispatchedCallSurvivesRefresh(t *testing.T) {
	for _, change := range []string{"description", "removal"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{}, 1)
			var refreshed atomic.Bool
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var req wire
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if req.ID == nil {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				var result any
				switch req.Method {
				case "initialize":
					result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
				case "tools/list":
					tools := []any{}
					if !refreshed.Load() || change != "removal" {
						description := "original"
						if refreshed.Load() {
							description = "changed"
						}
						tools = append(tools, map[string]any{"name": "effect", "description": description, "inputSchema": map[string]string{"type": "object"}})
					}
					result = map[string]any{"tools": tools}
				case "tools/call":
					if calls.Add(1) == 1 {
						close(entered)
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					result = map[string]any{"content": []any{}, "structuredContent": map[string]string{"receipt": "original-effect"}}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer srv.Close()
			m := New(t.Context(), t.TempDir(), nil)
			defer m.Close()
			defer close(release)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			candidate, err := m.Prepare(ctx, []Config{{Type: "http", Name: "fixture", URL: srv.URL}})
			if err != nil {
				t.Fatal(err)
			}
			defer candidate.Discard()
			if err := candidate.Commit(); err != nil {
				t.Fatal(err)
			}
			old := m.Snapshot()[0]
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := old.Call(ctx, `{}`)
				done <- outcome{result, err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("tools/call never reached HTTP peer")
			}
			refreshed.Store(true)
			if err := old.server.refresh(ctx); err != nil {
				t.Fatal(err)
			}
			// A retained approval must not invoke this obsolete definition again.
			if _, err := old.Call(ctx, `{}`); err == nil {
				t.Error("old binding dispatched after refresh")
			}
			select {
			case got := <-done:
				t.Fatalf("refresh ended accepted call before response release: %v; tools/call count=%d", got.err, calls.Load())
			case <-time.After(100 * time.Millisecond):
			}
			release <- struct{}{}
			select {
			case got := <-done:
				if got.err != nil || string(got.result.StructuredContent) != `{"receipt":"original-effect"}` || calls.Load() != 1 {
					t.Fatalf("result=%+v err=%v tools/call count=%d", got.result, got.err, calls.Load())
				}
			case <-ctx.Done():
				t.Fatal("accepted call lost its original result")
			}
		})
	}
}
