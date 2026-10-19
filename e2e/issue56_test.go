package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIssue56OpaqueContentEchoReplayAndModelProjection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(reply("ok"))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	raw := `[{"type":"_widget","text":{"value":"OPAQUE_SECRET"},"name":42,"uri":{"future":true}},{"type":"future_variant","text":{"value":"OPAQUE_FUTURE"},"name":false,"data":[],"resource":42}]`
	a.accepted(a.request("session/prompt", map[string]any{"sessionId": sid, "prompt": json.RawMessage(raw)}))
	updates := a.untilIdle(sid)
	users := filter(updates, "user_message")
	if len(users) != 1 {
		t.Fatalf("echo lost opaque block: %+v", updates)
	}
	assertJSON(t, users[0].Content, raw)
	model, _ := json.Marshal(llm.request(0).Messages)
	if strings.Contains(string(model), "OPAQUE_") {
		t.Fatalf("opaque block reached model text: %s", model)
	}
	a.stop()
	b := start(t, llm, withDataDir(dir))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := b.untilIdle(sid)
	users = filter(replay, "user_message")
	if len(users) != 1 {
		t.Fatalf("replay lost opaque block: %+v", replay)
	}
	assertJSON(t, users[0].Content, raw)
}
