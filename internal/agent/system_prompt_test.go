package agent

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

func TestUnsupportedSystemPromptOnNew(t *testing.T) {
	c := startAgent(t, t.TempDir(), nil, nil)
	c.initialize()
	cwd := t.TempDir()
	for _, tc := range []struct{ name, prompt string }{
		{"append", `{"mode":"append","content":[{"type":"text","text":"client text"}]}`},
		{"override", `{"mode":"override","content":[{"type":"text","text":"client text"}]}`},
		{"custom mode", `{"mode":"_custom","content":[{"type":"text","text":"client text"}]}`},
		{"custom content", `{"mode":"append","content":[{"type":"_custom"}]}`},
		{"empty object", `{}`},
		{"non-object", `false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := struct {
				Cwd          string          `json:"cwd"`
				SystemPrompt json.RawMessage `json:"systemPrompt"`
				Extension    any             `json:"SystemPrompt"`
			}{Cwd: cwd, SystemPrompt: json.RawMessage(tc.prompt)}
			r := c.await(c.send(acp.MethodSessionNew, params))
			if r.Error == nil || r.Error.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("session/new: got %s, %+v; want invalid params", r.Result, r.Error)
			}
		})
	}
}

func TestUnsupportedSystemPromptWhileReplayPending(t *testing.T) {
	gate := &gatedAcquireStore{entered: make(chan struct{}, 1), release: make(chan error, 1)}
	defer close(gate.release)
	c := startAgent(t, t.TempDir(), nil, nil, func(st Store) Store { gate.Store = st; return gate })
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	params := map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}}
	replay := c.send(acp.MethodSessionResume, params)
	issue50Wait(t, gate.entered)

	params["systemPrompt"] = json.RawMessage(`{"mode":"append","content":[{"type":"text","text":"client text"}]}`)
	c.fails(acp.MethodSessionResume, params, jsonrpc.CodeInvalidParams)
	delete(params, "systemPrompt")
	c.fails(acp.MethodSessionResume, params, replayConflict)

	gate.release <- nil
	if r := c.await(replay); r.Error != nil {
		t.Fatalf("original replay failed: %+v", r.Error)
	}
	got := c.untilIdle(sid)
	var kinds []string
	for _, u := range got {
		kinds = append(kinds, u.Update.Kind)
	}
	if !slices.Equal(kinds, []string{"replay_complete", "session_info_update", "state_update"}) {
		t.Fatalf("original replay updates = %v", kinds)
	}
}
