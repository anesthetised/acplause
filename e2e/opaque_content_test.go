package e2e

// Regression tests for #56: opaque content echo, replay and model projection.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssue56OpaqueContentEchoReplayAndModelProjection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t)
	llm.enqueue(reply("ok"))
	a, sid, cwd := started(t, llm, withDataDir(dir))
	raw := `[{"type":"text","text":"hello","name":42,"uri":{"value":"OPAQUE_SECRET"}},{"type":"resource_link","uri":"file:///w/x","name":"x","text":{"value":"OPAQUE_FUTURE"},"data":[],"resource":42}]`
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

	// Older agents admitted unknown block types. Replay must still read them
	// from the transcript even though new prompts containing them are rejected.
	const legacy = `[{"type":"_widget","text":{"value":"OPAQUE_SECRET"},"name":42,"uri":{"future":true}},{"type":"future_variant","text":{"value":"OPAQUE_FUTURE"},"name":false,"data":[],"resource":42}]`
	db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE transcript SET data = json_set(data, '$.update.content', json(?))
		WHERE session_id = ? AND json_extract(data, '$.kind') = 'user_message'`, legacy, sid); err != nil {
		t.Fatal(err)
	}

	b := start(t, llm, withDataDir(dir))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := b.untilIdle(sid)
	users = filter(replay, "user_message")
	if len(users) != 1 {
		t.Fatalf("replay lost opaque block: %+v", replay)
	}
	assertJSON(t, users[0].Content, legacy)
}
