package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPHTTPOwnedHeaders(t *testing.T) {
	requests := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	transport := newHTTP(nil, Config{URL: srv.URL, Headers: map[string]string{
		"aCCePt":               "text/plain",
		"coNTenT-TypE":         "text/plain",
		"mCP-pROTOcOL-veRSION": "configured-version",
		"mcP-sESSion-ID":       "configured-session",
		"lASt-eVENt-ID":        "configured-cursor",
		"hOST":                 "configured-host",
		"cONteNT-LeNGth":       "9999",
		"tRaNSFEr-eNCODInG":    "chunked",
		"cONNeCTiON":           "close",
		"aUTHoRIZaTiON":        "Bearer fixture",
		"x-aPI-kEY":            "fixture-key",
	}})
	t.Cleanup(transport.client.CloseIdleConnections)

	for _, tt := range []struct {
		name, method, version, session, cursor, accept, contentType string
		body                                                        []byte
	}{
		{name: "initialize", method: http.MethodPost, accept: "application/json, text/event-stream", contentType: "application/json", body: []byte("{}")},
		{name: "stateless GET", method: http.MethodGet, version: "2025-06-18", accept: "text/event-stream"},
		{name: "reconnect GET", method: http.MethodGet, version: "2025-06-18", session: "server-session", cursor: "event-cursor", accept: "text/event-stream"},
		{name: "close DELETE", method: http.MethodDelete, version: "2025-06-18", session: "server-session", accept: "application/json, text/event-stream"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transport.Initialized(tt.version)
			transport.sessionID = tt.session
			resp, err := transport.request(t.Context(), tt.method, tt.body, tt.cursor, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			r := <-requests
			for name, want := range map[string]string{
				"Accept":               tt.accept,
				"Content-Type":         tt.contentType,
				"MCP-Protocol-Version": tt.version,
				"Mcp-Session-Id":       tt.session,
				"Last-Event-ID":        tt.cursor,
				"Connection":           "",
				"Authorization":        "Bearer fixture",
				"X-API-Key":            "fixture-key",
			} {
				if got := r.Header.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if r.Host != strings.TrimPrefix(srv.URL, "http://") || r.ContentLength != int64(len(tt.body)) || len(r.TransferEncoding) != 0 {
				t.Errorf("request framing changed: host=%q length=%d transfer=%v", r.Host, r.ContentLength, r.TransferEncoding)
			}
		})
	}
}

func TestMCPHTTPDroppedResultNeverRepeatsCall(t *testing.T) {
	for _, cursor := range []bool{true, false} {
		t.Run(fmt.Sprint("cursor=", cursor), func(t *testing.T) {
			var posts atomic.Int32
			var deletes atomic.Int32
			var mu sync.Mutex
			var callID json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer configured" {
					t.Error("missing configured auth")
				}
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					w.WriteHeader(204)
					return
				}
				if r.Method == http.MethodGet {
					if r.Header.Get("Last-Event-ID") == "cursor-1" {
						w.Header().Set("Content-Type", "text/event-stream")
						mu.Lock()
						id := append(json.RawMessage(nil), callID...)
						mu.Unlock()
						result, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"content": []any{}, "structuredContent": map[string]string{"taskId": "task-opaque"}}})
						fmt.Fprintf(w, "data: %s\n\n", result)
						return
					}
					w.WriteHeader(405)
					return
				}
				var req wire
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "session-opaque" || r.Header.Get("MCP-Protocol-Version") != "2025-11-25") {
					t.Error("missing session/version header", r.Header)
				}
				if req.ID == nil {
					w.WriteHeader(202)
					return
				}
				var result any
				switch req.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "session-opaque")
					result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
				case "tools/list":
					result = map[string]any{"tools": []any{map[string]any{"name": "delegate_task", "inputSchema": map[string]string{"type": "object"}}}}
				case "tools/call":
					posts.Add(1)
					mu.Lock()
					callID = append(json.RawMessage(nil), req.ID...)
					mu.Unlock()
					w.Header().Set("Content-Type", "text/event-stream")
					if cursor {
						fmt.Fprint(w, ": keepalive\nid: cursor-1\nretry: 1\ndata:\n\n")
					} else {
						fmt.Fprint(w, ": keepalive\ndata:\n\n")
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer srv.Close()
			m := New(context.Background(), t.TempDir(), nil)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			change, err := m.Prepare(ctx, []Config{{Type: "http", Name: "s", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer configured"}}})
			if err != nil {
				m.Close()
				t.Fatal(err)
			}
			if err := change.Commit(); err != nil {
				t.Fatal(err)
			}
			result, err := m.Snapshot()[0].Call(ctx, `{"nested":{"task":true}}`)
			if cursor && (err != nil || string(result.StructuredContent) != `{"taskId":"task-opaque"}`) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !cursor && (err == nil || !strings.Contains(err.Error(), "outcome unknown")) {
				t.Fatalf("ambiguous call did not fail: %v", err)
			}
			m.Close()
			if posts.Load() != 1 || deletes.Load() != 1 {
				t.Fatalf("posts=%d deletes=%d", posts.Load(), deletes.Load())
			}
		})
	}
}

func TestMCPHTTPZeroRetryBacksOff(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "retry: 0\n\n") // and close, again and again
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		result := map[string]any{"tools": []any{}}
		if req.Method == "initialize" {
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	change, err := m.Prepare(ctx, []Config{{Type: "http", Name: "s", URL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if err := change.Commit(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	// Floors of 100, 200, 400 and 800 ms allow about four reconnections.
	if n := gets.Load(); n == 0 || n > 8 {
		t.Fatalf("%d GETs in a second", n)
	}
}

func TestMCPHTTPDoesNotFollowRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assertMCPSetupFailure(t, m, ctx, Config{Type: "http", Name: "s", URL: source.URL, Headers: map[string]string{"Authorization": "secret"}}, "could not initialize connection")
	if leaked.Load() {
		t.Fatal("credentials redirected")
	}
}

func TestMCPHTTPRejectsSetupFailures(t *testing.T) {
	for _, failure := range []string{"auth", "legacy", "size"} {
		t.Run(failure, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch failure {
				case "auth":
					w.WriteHeader(http.StatusUnauthorized)
				case "legacy":
					w.WriteHeader(http.StatusMethodNotAllowed)
				case "size":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, strings.Repeat("x", maxMessage+1))
				}
			}))
			defer srv.Close()
			m := New(context.Background(), t.TempDir(), nil)
			defer m.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			assertMCPSetupFailure(t, m, ctx, Config{Type: "http", Name: "s", URL: srv.URL}, "could not initialize connection")
		})
	}
}

func TestMCPHTTPExpiredSessionRecoversForFutureCall(t *testing.T) {
	var initialized atomic.Int32
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", fmt.Sprint(initialized.Add(1)))
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "tool", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			result = map[string]any{"content": []any{}, "structuredContent": map[string]string{"taskId": "new-session"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	change, err := m.Prepare(ctx, []Config{{Type: "http", Name: "s", URL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if err = change.Commit(); err != nil {
		t.Fatal(err)
	}
	old := m.Snapshot()[0]
	pending, err := m.Prepare(ctx, []Config{{Type: "http", Name: "s", URL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Discard()
	if _, err = old.Call(ctx, `{}`); err == nil {
		t.Fatal("expired session succeeded")
	}
	if calls.Load() != 1 {
		t.Fatal("expired operation repeated")
	}
	m.Recover(ctx)
	recovered := m.Snapshot()[0]
	if err := pending.Commit(); err != nil {
		t.Fatalf("recovery invalidated valid configuration: %v", err)
	}
	if m.Snapshot()[0].server != recovered.server {
		t.Fatal("commit restored obsolete connection")
	}
	if initialized.Load() != 2 {
		t.Fatalf("no new session: %d", initialized.Load())
	}
	if _, err = old.Call(ctx, `{}`); err == nil {
		t.Fatal("old binding resurrected")
	}
	result, err := m.Snapshot()[0].Call(ctx, `{}`)
	if err != nil || string(result.StructuredContent) != `{"taskId":"new-session"}` || calls.Load() != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}

func TestMCPHTTPFailureDoesNotRevokeOtherCalls(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "tool", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			calls.Add(1)
			if strings.Contains(string(req.Params), `"slow":true`) {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			if strings.Contains(string(req.Params), `"fail":true`) {
				w.WriteHeader(500)
				return
			}
			result = map[string]any{"content": []any{}, "structuredContent": map[string]bool{"ok": true}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	change, err := m.Prepare(ctx, []Config{{Type: "http", Name: "s", URL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if err = change.Commit(); err != nil {
		t.Fatal(err)
	}
	tool := m.Snapshot()[0]
	done := make(chan error, 1)
	go func() { _, err := tool.Call(ctx, `{"slow":true}`); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("slow call not started")
	}
	_, badErr := tool.Call(ctx, `{"fail":true}`)
	close(release)
	if badErr == nil {
		t.Fatal("HTTP 500 succeeded")
	}
	if err := <-done; err != nil {
		t.Fatalf("unrelated in-flight call failed: %v", err)
	}
	if _, err := tool.Call(ctx, `{}`); err != nil {
		t.Fatalf("later call failed: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls repeated or dropped: %d", calls.Load())
	}
}
