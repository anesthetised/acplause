package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLauncherDefaultRunsFirstPrompt(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("first answer"))
	a := start(t, llm, withEnv("ACPLAUSE_DEFAULT_MODEL=test/model"))
	a.initialize()
	session := a.newSession(t.TempDir())
	model := configByID(t, session.ConfigOptions, "model")
	effort := configByID(t, session.ConfigOptions, "reasoning_effort")
	if model.CurrentValue != "test/model" || !slices.Equal(values(model), []string{"test/model", "test/plain"}) || effort.CurrentValue != "default" {
		t.Fatalf("initial configuration: %+v", session.ConfigOptions)
	}
	a.accepted(a.prompt(session.SessionID, "first prompt"))
	a.untilIdle(session.SessionID)
	if req := llm.request(0); req.Model != "test/model" || req.Reasoning != nil {
		t.Fatalf("first prompt used model %q and reasoning %+v", req.Model, req.Reasoning)
	}
}

func TestNewSessionRequiresAuthorizedModel(t *testing.T) {
	for _, model := range []string{"", "test/removed", "test/notools"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			var mcpRequests atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mcpRequests.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(peer.Close)
			llm := newMockLLM(t)
			a := start(t, llm, withEnv("ACPLAUSE_DEFAULT_MODEL="+model))
			a.initialize()
			e := a.fails("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{
				map[string]any{"type": "http", "name": "observer", "url": peer.URL},
			}}, -32603)
			if !strings.Contains(e.Message, "ACPLAUSE_DEFAULT_MODEL") {
				t.Fatalf("missing launcher guidance: %+v", e)
			}
			var listed listResult
			a.ok("session/list", map[string]any{}, &listed)
			if len(listed.Sessions) != 0 || mcpRequests.Load() != 0 || llm.requestCount() != 0 {
				t.Fatalf("failed setup left sessions or contacted MCP or chat: %+v", listed)
			}
		})
	}
}

func TestCancelledModelSetupLeavesNoSession(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected provider request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		close(started)
		<-release
		fmt.Fprint(w, modelsJSON)
	}))
	t.Cleanup(peer.Close)
	var mcpRequests atomic.Int32
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mcpRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(mcp.Close)
	a := start(t, newMockLLM(t), withEnv("ACPLAUSE_BASE_URL="+peer.URL, "ACPLAUSE_DEFAULT_MODEL=test/model"))
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	a.initialize()
	waitClosed(t, started, "model catalog load did not start")
	id := a.request("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{
		map[string]any{"type": "http", "name": "observer", "url": mcp.URL},
	}})
	rpcID, _ := strconv.Atoi(id)
	a.notify("$/cancel_request", map[string]any{"requestId": rpcID})
	var listed listResult
	a.ok("session/list", map[string]any{}, nil) // Fence cancellation before releasing the catalog.
	unblock()
	if r := a.response(id); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("cancelled setup: %+v", r.Error)
	}
	a.ok("session/list", map[string]any{}, &listed)
	if len(listed.Sessions) != 0 || mcpRequests.Load() != 0 {
		t.Fatalf("cancelled setup left sessions or contacted MCP: %+v", listed)
	}
	// The successful listing remains cached and can serve an uncancelled request.
	a.newSession(t.TempDir())
}

func TestColdResumeReplacesUnavailableModel(t *testing.T) {
	t.Parallel()
	dir, cwd := t.TempDir(), t.TempDir()
	original := newMockLLM(t)
	original.enqueue(reply("saved answer"))
	a := start(t, original, withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL=test/model"))
	a.initialize()
	sid := a.newSession(cwd).SessionID
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "reasoning_effort", "type": "id", "value": "high"}, nil)
	a.accepted(a.prompt(sid, "saved question"))
	a.untilIdle(sid)
	other := a.newSession(t.TempDir()).SessionID
	a.ok("session/set_config_option", map[string]any{"sessionId": other, "configId": "model", "type": "id", "value": "test/plain"}, nil)
	a.stop()

	llm := newMockLLM(t, `{"data":[{"id":"test/plain","supported_parameters":["tools"]},{"id":"test/replacement","supported_parameters":["tools","reasoning"]}]}`)
	params := map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}}
	b := start(t, llm, withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL="))
	b.initialize()
	if e := b.fails("session/resume", params, -32603); !strings.Contains(e.Message, "ACPLAUSE_DEFAULT_MODEL") {
		t.Fatalf("missing launcher guidance: %+v", e)
	}
	if slices.ContainsFunc(b.pending, func(m *message) bool { return m.Method == "session/update" && sessionOf(m) == sid }) {
		t.Fatal("failed restore emitted replay or state updates")
	}
	b.expectQuiet(func(m *message) bool { return m.Method == "session/update" && sessionOf(m) == sid })
	b.stop()

	llm.enqueue(reply("new answer"))
	c := start(t, llm, withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL=test/replacement"))
	c.initialize()
	var resumed newSessionResult
	c.ok("session/resume", params, &resumed)
	replay := c.untilIdle(sid)
	if !slices.Equal(kinds(replay), []string{"user_message", "agent_message", "replay_complete", "session_info_update", "state_update idle"}) || replay[0].text() != "saved question" || replay[1].text() != "saved answer" {
		t.Fatalf("restore replay: %v", replay)
	}
	if configByID(t, resumed.ConfigOptions, "model").CurrentValue != "test/replacement" || configByID(t, resumed.ConfigOptions, "reasoning_effort").CurrentValue != "default" {
		t.Fatalf("replacement configuration: %+v", resumed.ConfigOptions)
	}
	c.accepted(c.prompt(sid, "next question"))
	c.untilIdle(sid)
	req := llm.request(0)
	if req.Model != "test/replacement" || req.Reasoning != nil || len(req.Messages) != 4 || req.Messages[1]["content"] != "saved question" {
		t.Fatalf("replacement prompt: %+v", req)
	}
	c.stop()

	d := start(t, llm, withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL="))
	d.initialize()
	d.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, &resumed)
	if got := configByID(t, resumed.ConfigOptions, "model").CurrentValue; got != "test/replacement" {
		t.Fatalf("restored model was not persisted: %q", got)
	}
	if got := configByID(t, d.newSession(cwd).ConfigOptions, "model").CurrentValue; got != "test/plain" {
		t.Fatalf("restore changed the last explicit choice: %q", got)
	}
}

func TestRememberLastSelection(t *testing.T) {
	t.Parallel()
	dir, cwd := t.TempDir(), t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(reply("remembered"))
	a := start(t, llm, withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL=test/plain"))
	a.initialize()
	old := a.newSession(cwd).SessionID
	setModel(a, old)
	a.ok("session/set_config_option", map[string]any{"sessionId": old, "configId": "reasoning_effort", "type": "id", "value": "high"}, nil)

	// New sessions inherit explicit choices without another selection.
	otherCwd := t.TempDir()
	selected := a.newSession(otherCwd)
	if configByID(t, selected.ConfigOptions, "model").CurrentValue != "test/model" || configByID(t, selected.ConfigOptions, "reasoning_effort").CurrentValue != "high" {
		t.Fatalf("new session selection: %+v", selected.ConfigOptions)
	}
	a.stop()

	b := start(t, llm, withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL=test/plain"))
	b.initialize()
	restarted := b.newSession(cwd)
	if configByID(t, restarted.ConfigOptions, "model").CurrentValue != "test/model" || configByID(t, restarted.ConfigOptions, "reasoning_effort").CurrentValue != "high" {
		t.Fatalf("selection after restart: %+v", restarted.ConfigOptions)
	}
	b.accepted(b.prompt(restarted.SessionID, "use the remembered selection"))
	b.untilIdle(restarted.SessionID)
	if req := llm.request(0); req.Model != "test/model" || req.Reasoning == nil || req.Reasoning.Effort != "high" {
		t.Fatalf("remembered selection not used: %+v", req)
	}

	// Last selection wins across sessions, including one in another window.
	c := start(t, llm, withDataDir(dir))
	c.initialize()
	c.ok("session/resume", map[string]any{"sessionId": old, "cwd": cwd}, nil)
	c.ok("session/set_config_option", map[string]any{"sessionId": old, "configId": "model", "type": "id", "value": "test/plain"}, nil)
	c.stop()
	b.fails("session/set_config_option", map[string]any{"sessionId": restarted.SessionID, "configId": "model", "type": "id", "value": "unknown"}, -32602)
	b.ok("session/resume", map[string]any{"sessionId": selected.SessionID, "cwd": otherCwd}, nil)
	b.ok("session/close", map[string]any{"sessionId": selected.SessionID}, nil)
	b.ok("session/delete", map[string]any{"sessionId": old}, nil)
	latest := b.newSession(cwd)
	if len(latest.ConfigOptions) != 2 || latest.ConfigOptions[0].CurrentValue != "test/plain" {
		t.Fatalf("last selection was overwritten: %+v", latest.ConfigOptions)
	}
}

func TestModelListRecoversWithoutRestart(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.modelsDown.Store(true)
	a := start(t, llm)
	a.initialize()
	a.fails("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}, -32603)
	llm.modelsDown.Store(false)
	if session := a.newSession(t.TempDir()); session.SessionID == "" {
		t.Fatalf("session after recovery: %+v", session)
	}
}

func TestRememberedModelUnavailable(t *testing.T) {
	t.Parallel()
	for _, catalog := range []string{
		`{"data":[{"id":"test/plain","supported_parameters":["tools"]}]}`,
		`{"data":[{"id":"test/plain","supported_parameters":["tools"]},{"id":"test/model","supported_parameters":["reasoning"]}]}`,
	} {
		t.Run(catalog, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			a, _, _ := started(t, newMockLLM(t), withDataDir(dir))
			a.stop()

			llm := newMockLLM(t, catalog)
			b := start(t, llm, withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL=test/plain"))
			b.initialize()
			session := b.newSession(t.TempDir())
			model := configByID(t, session.ConfigOptions, "model")
			if len(session.ConfigOptions) != 2 || model.CurrentValue != "test/plain" || !slices.Equal(values(model), []string{"test/plain"}) {
				t.Fatalf("unavailable remembered model: %+v", session.ConfigOptions)
			}
			b.stop()

			// A temporarily unavailable catalog entry does not erase the choice.
			c := start(t, newMockLLM(t), withDataDir(dir), withEnv("ACPLAUSE_DEFAULT_MODEL="))
			c.initialize()
			if restored := c.newSession(t.TempDir()); restored.ConfigOptions[0].CurrentValue != "test/model" {
				t.Fatalf("selection was forgotten: %+v", restored.ConfigOptions)
			}
		})
	}
}
