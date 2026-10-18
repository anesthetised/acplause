package e2e

import (
	"slices"
	"strings"
	"testing"
)

type sessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}

type listResult struct {
	Sessions   []sessionInfo `json:"sessions"`
	NextCursor *string       `json:"nextCursor"`
}

func TestListResumeAndClose(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("Paris."))
	a, sid, cwd := started(t, llm)
	other := a.newSession(t.TempDir()).SessionID

	messageID, _ := a.accepted(a.prompt(sid, "Capital of France?\nAnswer briefly."))
	turn := a.untilIdle(sid)
	answerID := joinChunks(turn, "agent_message_chunk").id

	// list: most recent first, titles from the first prompt, cwd filter.
	var all, filtered listResult
	a.ok("session/list", map[string]any{}, &all)
	if len(all.Sessions) != 2 || all.Sessions[0].SessionID != sid || all.Sessions[0].Title != "Capital of France?" || all.NextCursor != nil {
		t.Fatalf("session/list: %+v", all)
	}
	a.ok("session/list", map[string]any{"cwd": cwd, "cursor": nil}, &filtered)
	if len(filtered.Sessions) != 1 || filtered.Sessions[0].SessionID != sid {
		t.Fatalf("session/list by cwd: %+v", filtered)
	}

	// resume with replay: history with the original ids, then a state
	// snapshot, then the response.
	resp := a.call("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}})
	if resp.Error != nil {
		t.Fatalf("resume: %+v", resp.Error)
	}
	replay := a.untilIdle(sid)
	want := []string{"user_message", "agent_message", "state_update idle end_turn"}
	if got := kinds(replay); !slices.Equal(got, want) {
		t.Fatalf("replay: %v", got)
	}
	if replay[0].MessageID != messageID || replay[1].MessageID != answerID || replay[1].text() != "Paris." {
		t.Fatalf("replayed messages: %+v", replay[:2])
	}
	if last := replay[len(replay)-1]; last.seq > resp.seq {
		t.Fatal("the resume response must follow the state snapshot")
	}

	// resume without replay still synchronises state.
	resp = a.call("session/resume", map[string]any{"sessionId": other, "cwd": a.cwdOf(other, all)})
	snapshot := a.nextUpdate(other)
	if resp.Error != nil || !snapshot.is("state_update") || snapshot.State != "idle" || snapshot.StopReason != "" || snapshot.seq > resp.seq {
		t.Fatalf("resume without replay: %+v, %+v", resp.Error, snapshot)
	}

	a.fails("session/resume", map[string]any{"sessionId": "nope", "cwd": cwd}, -32002)
	a.fails("session/resume", map[string]any{"sessionId": sid, "cwd": "/elsewhere"}, -32602)
	a.fails("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "_checkpoint"}}, -32602)

	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
	a.fails("session/close", map[string]any{"sessionId": sid}, -32002)
	a.fails("session/prompt", map[string]any{"sessionId": sid, "prompt": []any{}}, -32002)
	var after listResult
	a.ok("session/list", map[string]any{}, &after)
	if len(after.Sessions) != 1 || after.Sessions[0].SessionID != other {
		t.Fatalf("session/list after close: %+v", after)
	}
}

func (a *agent) cwdOf(sid string, list listResult) string {
	for _, s := range list.Sessions {
		if s.SessionID == sid {
			return s.Cwd
		}
	}
	a.t.Fatalf("session %s not listed", sid)
	return ""
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t))
	a.initialize()
	cwd := t.TempDir()
	for range 51 {
		a.newSession(cwd)
	}

	var first, second listResult
	a.ok("session/list", map[string]any{}, &first)
	if len(first.Sessions) != 50 || first.NextCursor == nil {
		t.Fatalf("first page: %d sessions, cursor %v", len(first.Sessions), first.NextCursor)
	}
	a.ok("session/list", map[string]any{"cursor": *first.NextCursor}, &second)
	if len(second.Sessions) != 1 || second.NextCursor != nil {
		t.Fatalf("second page: %+v", second)
	}
	if slices.ContainsFunc(first.Sessions, func(s sessionInfo) bool { return s.SessionID == second.Sessions[0].SessionID }) {
		t.Fatal("pages overlap")
	}
	a.fails("session/list", map[string]any{"cursor": "not a cursor!"}, -32602)
}

func TestCloseCancelsTheRunningTurn(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	aborted := make(chan struct{})
	llm.enqueue(waitFor(nil, aborted))
	a, sid, _ := started(t, llm)

	a.accepted(a.prompt(sid, "long"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	resp := a.call("session/close", map[string]any{"sessionId": sid})
	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "cancelled" || idle.seq > resp.seq {
		t.Fatalf("close must report idle(cancelled) before responding: %+v at line %d, response at %d", idle, idle.seq, resp.seq)
	}
	waitClosed(t, aborted, "the model request was not aborted")
}

func TestConfigOptions(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("one"), reply("two"))
	a := start(t, llm)
	a.initialize()
	session := a.newSession(t.TempDir())
	sid := session.SessionID

	model := configByID(t, session.ConfigOptions, "model")
	if len(session.ConfigOptions) != 1 || model.CurrentValue != "" || model.Category != "model" || !slices.Equal(values(model), []string{"", "test/model", "test/plain"}) || model.Options[0].Name != "Select a model" {
		t.Fatalf("model option: %+v", model)
	}
	// Reject before inserting the message or contacting the model.
	if resp := a.response(a.prompt(sid, "must not enter history")); resp.Error == nil || resp.Error.Code != -32602 || !strings.Contains(resp.Error.Message, "select a model") {
		t.Fatalf("prompt without a model: %+v", resp.Error)
	}
	a.fails("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": ""}, -32602)
	a.expectQuiet(func(m *message) bool { return m.Method == "session/update" })
	if llm.requestCount() != 0 {
		t.Fatal("prompt without a model reached the provider")
	}

	var res struct {
		ConfigOptions []configOption `json:"configOptions"`
	}
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, &res)
	model, effort := configByID(t, res.ConfigOptions, "model"), configByID(t, res.ConfigOptions, "reasoning_effort")
	if model.CurrentValue != "test/model" || !slices.Equal(values(model), []string{"test/model", "test/plain"}) || effort.CurrentValue != "default" || effort.Category != "thought_level" {
		t.Fatalf("after selecting a model: %+v", res.ConfigOptions)
	}
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "reasoning_effort", "type": "id", "value": "high"}, &res)
	if configByID(t, res.ConfigOptions, "reasoning_effort").CurrentValue != "high" {
		t.Fatalf("after setting effort: %+v", res.ConfigOptions)
	}
	a.accepted(a.prompt(sid, "think hard"))
	a.untilIdle(sid)
	if req := llm.request(0); req.Model != "test/model" || req.Reasoning == nil || req.Reasoning.Effort != "high" {
		t.Fatalf("request with effort: %+v", req)
	}
	if msgs := llm.request(0).Messages; len(msgs) != 2 || msgs[1]["content"] != "think hard" {
		t.Fatalf("rejected prompt entered history: %v", msgs)
	}

	// A model without reasoning drops the dependent option.
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/plain"}, &res)
	if len(res.ConfigOptions) != 1 || res.ConfigOptions[0].CurrentValue != "test/plain" {
		t.Fatalf("after switching model: %+v", res.ConfigOptions)
	}
	a.accepted(a.prompt(sid, "plain"))
	a.untilIdle(sid)
	if req := llm.request(1); req.Model != "test/plain" || req.Reasoning != nil {
		t.Fatalf("request after switching model: %+v", req)
	}

	for _, bad := range []map[string]any{
		{"configId": "model", "type": "id", "value": "test/notools"},
		{"configId": "reasoning_effort", "type": "id", "value": "high"},
		{"configId": "model", "type": "boolean", "value": true},
		{"configId": "temperature", "type": "id", "value": "1"},
	} {
		bad["sessionId"] = sid
		a.fails("session/set_config_option", bad, -32602)
	}
	a.expectQuiet(func(m *message) bool { return m.Method == "session/update" })
}

func TestSessionRequiresAvailableModels(t *testing.T) {
	t.Parallel()
	for _, catalog := range []string{`{"data":[]}`, `{"data":[{"id":"test/notools","supported_parameters":["temperature"]}]}`, `invalid JSON`} {
		t.Run(catalog, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, catalog)
			a := start(t, llm)
			a.initialize()
			e := a.fails("session/new", map[string]any{"cwd": t.TempDir()}, -32603)
			if !strings.Contains(e.Message, "model") {
				t.Fatalf("unhelpful catalog error: %+v", e)
			}
			var res listResult
			a.ok("session/list", map[string]any{}, &res)
			if len(res.Sessions) != 0 || llm.requestCount() != 0 {
				t.Fatal("failed session creation left a session or contacted chat completions")
			}
		})
	}
}

func configByID(t *testing.T, options []configOption, id string) configOption {
	t.Helper()
	for _, o := range options {
		if o.ConfigID == id {
			return o
		}
	}
	t.Fatalf("no %s option in %+v", id, options)
	return configOption{}
}

func values(o configOption) []string {
	var vs []string
	for _, v := range o.Options {
		vs = append(vs, v.Value)
	}
	return vs
}
