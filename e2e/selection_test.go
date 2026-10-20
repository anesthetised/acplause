package e2e

import "testing"

func TestRememberLastSelection(t *testing.T) {
	t.Parallel()
	dir, cwd := t.TempDir(), t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(reply("remembered"))
	a := start(t, llm, withDataDir(dir))
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

	b := start(t, llm, withDataDir(dir))
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
			b := start(t, llm, withDataDir(dir))
			b.initialize()
			session := b.newSession(t.TempDir())
			model := configByID(t, session.ConfigOptions, "model")
			if len(session.ConfigOptions) != 2 || model.CurrentValue != "" || model.Options[0].Name != "Select a model" {
				t.Fatalf("unavailable remembered model: %+v", session.ConfigOptions)
			}
			b.fails("session/prompt", map[string]any{"sessionId": session.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "do not pick a default"}}}, -32602)
			if llm.requestCount() != 0 {
				t.Fatal("prompt reached a substitute model")
			}
			b.stop()

			// A temporarily unavailable catalog entry does not erase the choice.
			c := start(t, newMockLLM(t), withDataDir(dir))
			c.initialize()
			if restored := c.newSession(t.TempDir()); restored.ConfigOptions[0].CurrentValue != "test/model" {
				t.Fatalf("selection was forgotten: %+v", restored.ConfigOptions)
			}
		})
	}
}
