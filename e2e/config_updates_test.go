package e2e

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func configRequest(sid, id, value string) map[string]string {
	return map[string]string{"sessionId": sid, "configId": id, "type": "id", "value": value}
}

func configSnapshot(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var snapshot struct {
		Options json.RawMessage `json:"configOptions"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil || len(snapshot.Options) == 0 {
		t.Fatalf("missing config snapshot in %s: %v", raw, err)
	}
	return snapshot.Options
}

func TestConfigUpdatesFollowPipelinedSetters(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("restored"))
	a := start(t, llm, withEnv("ACPLAUSE_DEFAULT_MODEL=test/plain"))
	a.initialize()
	cwd := t.TempDir()
	sid := a.newSession(cwd).SessionID
	set := func(id, value string) string {
		return a.request("session/set_config_option", configRequest(sid, id, value))
	}
	model := set("model", "test/model")
	effort := set("reasoning_effort", "high")
	same := set("reasoning_effort", "high")
	invalid := set("reasoning_effort", "_invalid")
	plain := set("model", "test/plain")
	plan := set("mode", "plan")

	next := func() *message { return a.take(func(*message) bool { return true }) }
	response := func(id string) *message {
		t.Helper()
		resp := next()
		if resp.Method != "" || string(resp.ID) != id {
			t.Fatalf("expected response %s, got %+v", id, resp)
		}
		return resp
	}

	changed := func(id string) []configOption {
		t.Helper()
		m := next()
		if m.Method != "session/update" || sessionOf(m) != sid || !decodeUpdate(t, m).is("config_option_update") {
			t.Fatalf("setter %s: expected config update first, got %+v", id, m)
		}
		var params struct{ Update json.RawMessage }
		if err := json.Unmarshal(m.Params, &params); err != nil {
			t.Fatal(err)
		}
		resp := response(id)
		if resp.Error != nil {
			t.Fatal(resp.Error)
		}
		if want := configSnapshot(t, resp.Result); !bytes.Equal(configSnapshot(t, params.Update), want) {
			t.Fatalf("update %s differs from response %s", params.Update, resp.Result)
		}
		var result newSessionResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result.ConfigOptions
	}

	initial := changed(model)
	if configByID(t, initial, "model").CurrentValue != "test/model" || configByID(t, initial, "reasoning_effort").CurrentValue != "default" {
		t.Fatalf("model snapshot: %+v", initial)
	}
	reasoning := changed(effort)
	sameResp, invalidResp := response(same), response(invalid)
	if sameResp.Error != nil || invalidResp.Error == nil || invalidResp.Error.Code != -32602 {
		t.Fatalf("same setter: %+v; invalid setter: %+v", sameResp, invalidResp)
	}
	if configByID(t, reasoning, "reasoning_effort").CurrentValue != "high" {
		t.Fatalf("effort snapshot: %+v", reasoning)
	}
	var sameResult newSessionResult
	if err := json.Unmarshal(sameResp.Result, &sameResult); err != nil || !reflect.DeepEqual(sameResult.ConfigOptions, reasoning) {
		t.Fatalf("same-value response: %s, %v", sameResp.Result, err)
	}
	if opts := changed(plain); len(opts) != 2 || configByID(t, opts, "model").CurrentValue != "test/plain" {
		t.Fatalf("plain model snapshot: %+v", opts)
	}
	if opts := changed(plan); configByID(t, opts, "mode").CurrentValue != "plan" {
		t.Fatalf("mode snapshot: %+v", opts)
	}

	a.ok("session/close", map[string]string{"sessionId": sid}, nil)
	var restored newSessionResult
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}}, &restored)
	if updates := a.untilIdle(sid); len(updates) != 3 || !updates[0].is("replay_complete") || !updates[1].is("session_info_update") {
		t.Fatalf("replay included historical configuration: %v", kinds(updates))
	}
	if len(restored.ConfigOptions) != 2 || configByID(t, restored.ConfigOptions, "model").CurrentValue != "test/plain" ||
		configByID(t, restored.ConfigOptions, "mode").CurrentValue != "plan" {
		t.Fatalf("cold resume configuration: %+v", restored.ConfigOptions)
	}
	a.accepted(a.prompt(sid, "use the restored model"))
	a.untilIdle(sid)
	if req := llm.request(0); req.Model != "test/plain" || req.Reasoning != nil {
		t.Fatalf("cold resume sent model %q with reasoning %+v", req.Model, req.Reasoning)
	}
}

func TestConfigSettersWaitForReplayAndCancelledSetterIsSilent(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("first"), reply("second"))
	a := start(t, llm, withEnv("ACPLAUSE_DEFAULT_MODEL=test/plain"))
	a.initialize()
	cwd := t.TempDir()
	sid := a.newSession(cwd).SessionID
	resumeID, release := gatedReplay(t, a, sid, cwd)
	cancelID := a.request("session/set_config_option", configRequest(sid, "model", "test/model"))
	a.notify("$/cancel_request", map[string]any{"requestId": requestID(t, cancelID)})
	setID := a.request("session/set_config_option", configRequest(sid, "model", "test/model"))
	prompt1 := a.prompt(sid, "first")
	prompt2 := a.prompt(sid, "second")
	a.fails("_test/barrier", nil, -32601)
	release()

	resume := a.response(resumeID)
	var before newSessionResult
	if resume.Error != nil || json.Unmarshal(resume.Result, &before) != nil || configByID(t, before.ConfigOptions, "model").CurrentValue != "test/plain" {
		t.Fatalf("resume config changed ahead of setup: %+v", resume)
	}
	if updates := a.untilIdle(sid); len(updates) != 3 || !updates[0].is("replay_complete") || !updates[1].is("session_info_update") || updates[2].seq >= resume.seq {
		t.Fatalf("invalid replay boundary: %v", kinds(updates))
	}
	cancelled := a.response(cancelID)
	if cancelled.Error == nil || cancelled.Error.Code != -32800 {
		t.Fatalf("queued setter was not cancelled: %+v", cancelled)
	}
	pushed := a.nextUpdate(sid)
	set := a.response(setID)
	if !pushed.is("config_option_update") || !(resume.seq < cancelled.seq && cancelled.seq < pushed.seq && pushed.seq < set.seq) || set.Error != nil {
		t.Fatalf("setter ordering: resume %d, cancellation %d, update %+v, response %+v", resume.seq, cancelled.seq, pushed, set)
	}

	for i, id := range []string{prompt1, prompt2} {
		msgID, resp := a.accepted(id)
		updates := a.untilIdle(sid)
		users := filter(updates, "user_message")
		if len(users) != 1 || users[0].MessageID != msgID || users[0].seq <= set.seq || resp.seq <= set.seq {
			t.Fatalf("prompt %d overtook the setter or the earlier prompt: %v", i, kinds(updates))
		}
		if got := llm.request(i).Model; got != "test/model" {
			t.Fatalf("prompt %d used model %q", i, got)
		}
	}
}
