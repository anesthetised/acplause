package e2e

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

func TestIssue54BatchPromptKeepsSingleResponseArray(t *testing.T) {
	t.Parallel()
	model := newMockLLM(t)
	model.enqueue(reply("done"))
	a, sid, _ := started(t, model)
	a.mu.Lock()
	a.methods[`"issue54-prompt"`] = "session/prompt"
	a.methods[`"issue54-initialize"`] = "initialize"
	a.mu.Unlock()
	a.write([]any{
		map[string]any{"jsonrpc": "2.0", "id": "issue54-prompt", "method": "session/prompt", "params": map[string]any{
			"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "batch prompt"}},
		}},
		map[string]any{"jsonrpc": "2.0", "id": "issue54-initialize", "method": "initialize", "params": map[string]any{
			"protocolVersion": 2, "info": map[string]any{"name": "e2e", "version": "1"},
		}},
	})
	messageID, prompt := a.accepted(`"issue54-prompt"`)
	initialized := a.response(`"issue54-initialize"`)
	if !prompt.batch || !initialized.batch || prompt.seq != initialized.seq || initialized.Error != nil {
		t.Fatalf("batch split or failed: prompt=%+v initialize=%+v", prompt, initialized)
	}
	user := find(t, a.untilIdle(sid), "user_message")
	if user.MessageID != messageID {
		t.Fatalf("batch acknowledged %q, user_message echoed %q", messageID, user.MessageID)
	}
}
