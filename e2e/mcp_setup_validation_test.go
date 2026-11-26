package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPSetupValidation(t *testing.T) {
	t.Parallel()
	const credential = "fixture-mcp-credential"
	const auth = "Bearer " + credential
	const ignored = "fixture-ignored-header-value"
	const upstreamSession = "fixture-upstream-session"
	var rejectedRequests, initializes, deletes atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rejected" {
			rejectedRequests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Header.Get("Authorization") != auth {
			t.Error("configured authentication did not reach the MCP peer")
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			ID     json.RawMessage
			Method string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Accept") != "application/json, text/event-stream" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Last-Event-ID") != "" {
			t.Error("wrong MCP representation or cursor headers")
		}
		var result any
		switch req.Method {
		case "initialize":
			initializes.Add(1)
			if r.Header.Get("Mcp-Session-Id") != "" || r.Header.Get("MCP-Protocol-Version") != "" {
				t.Error("initialize received a session ID or protocol version header")
			}
			w.Header().Set("Mcp-Session-Id", upstreamSession)
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "notifications/initialized", "tools/list":
			if r.Header.Get("Mcp-Session-Id") != upstreamSession || r.Header.Get("MCP-Protocol-Version") != "2025-11-25" {
				t.Error("MCP session ID or negotiated version was lost")
			}
			if req.Method == "notifications/initialized" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			result = map[string]any{"tools": []any{}}
		default:
			t.Errorf("unexpected MCP method %q", req.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(peer.Close)

	data := t.TempDir()
	a := start(t, newMockLLM(t), withDataDir(data))
	a.initialize()
	cwd := t.TempDir()
	candidate := map[string]any{"type": "http", "name": "candidate", "url": peer.URL + "/rejected"}
	invalid := [][]any{
		{candidate, map[string]any{"type": "sse", "name": "legacy", "url": peer.URL + "/rejected"}},
		{candidate, map[string]any{"type": "_future", "headers": ignored}},
		{candidate, candidate},
	}
	checkRejected := func(method string, params map[string]any) {
		t.Helper()
		for _, servers := range invalid {
			params["mcpServers"] = servers
			rpcErr := a.fails(method, params, -32602)
			if strings.Contains(rpcErr.Message, ignored) || bytes.Contains(rpcErr.Data, []byte(ignored)) {
				t.Fatal("rejected configuration appeared in the error")
			}
		}
		if n := rejectedRequests.Load(); n != 0 {
			t.Fatalf("invalid setup contacted the peer %d times", n)
		}
	}
	checkRejected("session/new", map[string]any{"cwd": cwd})
	var list listResult
	a.ok("session/list", map[string]any{}, &list)
	if len(list.Sessions) != 0 {
		t.Fatal("invalid setup created a session")
	}

	headers := []map[string]string{{"name": "aUtHoRiZaTiOn", "value": auth}}
	for _, name := range []string{"aCcEpT", "cOnTeNt-tYpE", "mCp-SeSsIoN-iD", "mCp-PrOtOcOl-VeRsIoN", "lAsT-eVeNt-Id"} {
		headers = append(headers, map[string]string{"name": name, "value": ignored})
	}
	config := func(path string) map[string]any {
		return map[string]any{"type": "http", "name": "fixture", "url": peer.URL + path, "headers": headers}
	}
	var created newSessionResult
	a.ok("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{config("/new")}}, &created)
	sid := created.SessionID
	checkRejected("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}})
	for _, m := range a.pending {
		if m.Method == "session/update" && sessionOf(m) == sid {
			t.Fatal("invalid resume sent a session update")
		}
	}
	if deletes.Load() != 0 {
		t.Fatal("invalid resume closed the active peer")
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{config("/resumed")}}, nil)
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
	a.stop()
	if initializes.Load() != 2 {
		t.Fatal("new and resumed sessions must each initialize their peer")
	}
	db, err := os.ReadFile(filepath.Join(data, "acplause.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{credential, ignored, upstreamSession} {
		if strings.Contains(a.stderr.String(), value) || bytes.Contains(db, []byte(value)) {
			t.Fatal("MCP header value leaked into stderr or the session database")
		}
	}
}
