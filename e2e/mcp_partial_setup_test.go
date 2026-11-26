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

func TestMCPPartialSetupNotices(t *testing.T) {
	t.Parallel()
	const credential = "mcp-setup-private-credential"
	const upstreamSID = "mcp-setup-private-session"
	const rawError = "mcp-setup-private-upstream-error"
	const privatePath = "/mcp-setup-private-path"
	var deletes atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential || r.URL.Path != privatePath {
			t.Error("wrong credentials or endpoint during setup")
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case http.MethodDelete:
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
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", upstreamSID)
			response["result"] = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			response["error"] = map[string]any{"code": -32603, "message": rawError}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		default:
			t.Errorf("unexpected MCP method %q", req.Method)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(peer.Close)
	llm := newMockLLM(t)
	dataDir := t.TempDir()
	a := start(t, llm, withDataDir(dataDir))
	a.initialize()
	cwd := t.TempDir()
	good := mcpConfig("good", filepath.Join(cwd, "calls"), "healthy")
	pidFile := filepath.Join(cwd, "pid")
	good["env"] = append(good["env"].([]any), map[string]string{"name": "FIXTURE_PID", "value": pidFile})
	badHTTP := map[string]any{"type": "http", "name": "mcp-setup-private-name", "url": peer.URL + privatePath, "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + credential}}}
	badStdio := map[string]any{"type": "stdio", "name": "bad-stdio", "command": filepath.Join(cwd, "mcp-setup-private-command"), "env": []any{map[string]string{"name": "PRIVATE", "value": "mcp-setup-private-env"}}}
	open := func(configs []any) (string, int) {
		t.Helper()
		resp := a.response(a.request("session/new", map[string]any{"cwd": cwd, "mcpServers": configs}))
		var created newSessionResult
		if err := json.Unmarshal(resp.Result, &created); resp.Error != nil || err != nil || created.SessionID == "" {
			t.Fatalf("new session failed: %+v, %v", resp, err)
		}
		return created.SessionID, resp.seq
	}
	assertNotice := func(sid string, after int, title, detail string) {
		t.Helper()
		u := a.nextUpdate(sid)
		if !u.is("notice") || u.seq <= after || u.Severity != "warning" || u.Title != title || u.Description != detail {
			t.Fatalf("bad setup warning after line %d: %+v", after, u)
		}
	}
	sid, responseSeq := open([]any{badHTTP, good, badStdio})
	assertNotice(sid, responseSeq, "MCP setup incomplete", "Server 1 (http) could not discover tools.\nServer 3 (stdio) could not launch server.\nConnected MCP servers remain available.")
	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
	llm.enqueue(mcpCall(t, "good", "delegate_task", `{}`), reply("done"))
	a.accepted(a.prompt(sid, "use available tools"))
	a.untilIdle(sid)
	assertMCPSetupTools(t, llm.request(0), "good")

	replay := map[string]string{"type": "start"}
	resume := func(configs []any) int {
		t.Helper()
		r := a.response(a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": configs, "replayFrom": replay}))
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		var user, marker, info, state int
		for {
			u := a.nextUpdate(sid)
			switch u.SessionUpdate {
			case "notice":
				t.Fatal("setup notice appeared in history or before synchronization")
			case "user_message":
				user = u.seq
			case "replay_complete":
				marker = u.seq
			case "session_info_update":
				info = u.seq
			case "state_update":
				state = u.seq
			}
			if state != 0 {
				break
			}
		}
		if user == 0 || !(user < marker && marker < info && info < state && state < r.seq) {
			t.Fatalf("wrong resume sequence: history=%d marker=%d info=%d state=%d response=%d", user, marker, info, state, r.seq)
		}
		return r.seq
	}
	responseSeq = resume([]any{good, badHTTP})
	assertNotice(sid, responseSeq, "MCP setup incomplete", "Server 2 (http) could not discover tools.\nConnected MCP servers remain available.")
	resume([]any{good})
	if got, err := os.ReadFile(pidFile); err != nil || !bytes.Equal(got, pid) {
		t.Fatalf("unchanged peer was restarted: pid %q -> %q, %v", pid, got, err)
	}

	allFailed, responseSeq := open([]any{badHTTP})
	assertNotice(allFailed, responseSeq, "MCP servers unavailable", "Server 1 (http) could not discover tools.\nThe session will use built-in tools.")
	llm.enqueue(reply("built-ins available"))
	a.accepted(a.prompt(allFailed, "continue without MCP"))
	a.untilIdle(allFailed)
	assertMCPSetupTools(t, llm.request(2), "")
	a.stop()
	if deletes.Load() != 3 {
		t.Fatalf("cleaned up %d failed HTTP peers, want 3", deletes.Load())
	}
	db, err := os.ReadFile(filepath.Join(dataDir, "acplause.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"mcp-setup-private-", peer.URL, rawError, upstreamSID, credential} {
		if strings.Contains(a.stderr.String(), private) || bytes.Contains(db, []byte(private)) {
			t.Fatal("setup details leaked into stderr or storage")
		}
	}
}

// Verify the model received built-ins and only the expected MCP peer, if any.
func assertMCPSetupTools(t *testing.T, req chatRequest, peer string) {
	t.Helper()
	builtin, external := false, false
	for _, tool := range req.Tools {
		name, _ := tool["function"].(map[string]any)["name"].(string)
		builtin = builtin || name == "read_file"
		if strings.HasPrefix(name, "mcp_") {
			external = true
			if peer == "" || !strings.HasPrefix(name, "mcp_"+peer+"_") {
				t.Fatal("unexpected MCP tool in model request")
			}
		}
	}
	if !builtin || external != (peer != "") {
		t.Fatal("wrong tools offered to the model after MCP setup")
	}
}
