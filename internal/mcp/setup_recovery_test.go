package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSetupRecoveryDuringPreparation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "different header"}[changed], func(t *testing.T) {
			testSetupRecoveryDuringPreparation(t, changed)
		})
	}
}

func testSetupRecoveryDuringPreparation(t *testing.T, changed bool) {
	t.Helper()
	var initialized atomic.Int32
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
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
			attempt := initialized.Add(1)
			if attempt == 2 {
				close(entered)
				select {
				case <-release:
					w.WriteHeader(http.StatusServiceUnavailable)
				case <-r.Context().Done():
				}
				return
			}
			w.Header().Set("Mcp-Session-Id", fmt.Sprint(attempt))
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
	defer unblock()
	m := New(context.Background(), t.TempDir(), nil)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := Config{Type: "http", Name: "s", URL: srv.URL, Headers: map[string]string{"Authorization": "original", "X-Fixture": "fixture"}}
	change, err := m.Prepare(ctx, []Config{cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err = change.Commit(); err != nil {
		t.Fatal(err)
	}
	old := m.Snapshot()[0]
	if _, err := old.Call(ctx, `{}`); err == nil {
		t.Fatal("expired session succeeded")
	}
	type prepared struct {
		change *Change
		err    error
	}
	result := make(chan prepared, 1)
	requested := cfg
	requested.Headers = map[string]string{"X-Fixture": "fixture", "Authorization": "original"}
	if changed {
		requested.Headers["Authorization"] = "replacement"
	}
	go func() {
		change, err := m.Prepare(ctx, []Config{requested})
		result <- prepared{change, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("resume attempt did not reach the fixture")
	}
	m.Recover(ctx)
	recovered := m.Snapshot()[0]
	if recovered.Context().Err() != nil {
		t.Fatal("recovery did not install a working connection")
	}
	unblock()
	pending := <-result
	if pending.err != nil {
		t.Fatal(pending.err)
	}
	defer pending.change.Discard()
	if err := pending.change.Commit(); err != nil {
		t.Fatal(err)
	}
	failures := pending.change.Failures()
	if changed {
		if len(m.Snapshot()) != 0 || recovered.Context().Err() == nil ||
			len(failures) != 1 || failures[0].Reason != errSetupInitialize.Error() {
			t.Fatalf("failed replacement reused a peer with different credentials: %v, failures=%+v", recovered.Context().Err(), failures)
		}
		return
	}
	available := m.Snapshot()
	if len(available) != 1 || recovered.Context().Err() != nil || len(failures) != 0 {
		t.Fatalf("failed retry retired an unchanged recovered peer: %v, failures=%+v", recovered.Context().Err(), failures)
	}
	if result, err := available[0].Call(ctx, `{}`); err != nil || string(result.StructuredContent) != `{"taskId":"new-session"}` {
		t.Fatalf("recovered MCP binding could not run: %+v, %v", result, err)
	}
}
