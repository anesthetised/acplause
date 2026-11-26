package e2e

// Regression tests for #54: prompt response precedes the user message.

import "testing"

func TestIssue54PromptResponsePrecedesUserMessage(t *testing.T) {
	t.Parallel()
	model := newMockLLM(t)
	model.enqueue(reply("done"))
	a, sid, _ := started(t, model)
	messageID, response := a.accepted(a.prompt(sid, "acknowledge before echoing"))
	updates := a.untilIdle(sid)
	user := find(t, updates, "user_message")
	if user.MessageID != messageID || response.seq >= user.seq {
		t.Fatalf("prompt response %q at line %d must precede user_message %q at line %d", messageID, response.seq, user.MessageID, user.seq)
	}
	for _, update := range updates {
		if update.seq <= response.seq {
			t.Errorf("turn update %s at line %d overtook prompt response at line %d", update, update.seq, response.seq)
		}
	}
}

func TestArrayPromptDoesNotStartATurn(t *testing.T) {
	t.Parallel()
	model := newMockLLM(t)
	model.enqueue(reply("done"))
	a, sid, _ := started(t, model)
	a.write([]any{map[string]any{"jsonrpc": "2.0", "id": "array-prompt", "method": "session/prompt", "params": map[string]any{
		"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "embedded"}},
	}}})
	if m := a.response("null"); m.Error == nil || m.Error.Code != -32600 {
		t.Fatalf("array prompt response: %+v", m)
	}
	messageID, _ := a.accepted(a.prompt(sid, "standalone"))
	user := find(t, a.untilIdle(sid), "user_message")
	if user.MessageID != messageID || user.text() != "standalone" || model.requestCount() != 1 {
		t.Fatalf("first turn echoed %q (%s), want standalone (%s) with one model request; got %d requests", user.text(), user.MessageID, messageID, model.requestCount())
	}
}
