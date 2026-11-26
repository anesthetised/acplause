package e2e

import (
	"encoding/json"
	"testing"
)

func TestPromptContentAdmissionAndReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(reply("ok"))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	params := func(prompt string) map[string]any {
		return map[string]any{"sessionId": sid, "idempotencyKey": "submission", "prompt": json.RawMessage(prompt)}
	}
	resume := func(a *agent) []update {
		a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
		return a.untilIdle(sid)
	}
	for _, block := range []string{
		`{"type":"audio","mimeType":"audio/wav","data":"AA=="}`,
		`{"type":"_vendor_block","payload":7}`,
		`{"type":"future_block","payload":7}`,
		`{"type":"resource_link","uri":"file:///w/x","name":"x","size":-1}`,
		`{"type":"text","text":null}`,
	} {
		a.fails("session/prompt", params(`[{"type":"text","text":"reject the whole prompt"},`+block+`]`), -32602)
	}
	if got := resume(a); len(got) != 3 || !got[0].is("replay_complete") || !got[1].is("session_info_update") || got[1].Title != "" {
		t.Fatalf("rejected prompts emitted updates or entered replay: %v", kinds(got))
	}
	if got := llm.requestCount(); got != 0 {
		t.Fatalf("rejected prompts made %d model requests", got)
	}

	const valid = `[{"type":"text","text":"review this","Text":false,"annotations":{"audience":["_custom","future_role"],"priority":1,"future":true},"_meta":{"example.test/data":9007199254740993}},{"type":"resource_link","uri":"file:///w/x","name":"x","size":0,"future":{"value":true}},{"type":"resource","resource":{"uri":"file:///w/blob","blob":"AA==","text":42}}]`
	id, _ := a.accepted(a.request("session/prompt", params(valid)))
	checkUserMessage := func(updates []update) {
		t.Helper()
		users := filter(updates, "user_message")
		if len(users) != 1 || users[0].MessageID != id || string(users[0].Content) != valid {
			t.Fatalf("user message changed: %+v", users)
		}
	}
	checkUserMessage(a.untilIdle(sid))
	if got := llm.request(0).Messages[1]["content"]; got != "review this\n\n[x](file:///w/x)\n\n[binary resource omitted: file:///w/blob]" {
		t.Fatalf("model projection = %q", got)
	}
	a.fails("session/prompt", params(`[{"type":"_vendor_block"}]`), -32602)
	if retry, _ := a.accepted(a.request("session/prompt", params(`[{"type":"text","text":"different valid content"}]`))); retry != id {
		t.Fatalf("retry ID = %q, want %q", retry, id)
	}
	a.stop()

	b := start(t, llm, withDataDir(dir))
	b.initialize()
	checkUserMessage(resume(b))
	if got := llm.requestCount(); got != 1 {
		t.Fatalf("model requests = %d, want 1", got)
	}
}
