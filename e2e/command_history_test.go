package e2e

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactClassifiesPromptsAfterRestart(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "ordinary multipart"}[multipart], func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			llm := newMockLLM(t, compactionModels)
			a, sid, cwd := started(t, llm, withDataDir(dir))
			prompt := "/compact"
			if multipart {
				prompt = "ordinary prompt"
				llm.enqueue(reply(strings.Repeat("ordinary answer ", 100)))
			}
			original, _ := a.accepted(a.prompt(sid, prompt))
			a.untilIdle(sid)
			a.stop()

			// Recreate the text and blocks that an older agent wrote. Leave its
			// stored command marker in place for the old single-block command.
			legacy, historyText := `[{"type":"text","text":" /compact"}]`, " /compact"
			if multipart {
				legacy, historyText = `[{"type":"text","text":"/compact"},{"type":"text","text":""}]`, "/compact\n\n"
			}
			db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE transcript SET data = json_set(data, '$.update.content', json(?))
				WHERE session_id = ? AND json_extract(data, '$.kind') = 'user_message'`, legacy, sid); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE history SET data = json_set(data, '$.content', ?)
				WHERE session_id = ? AND json_extract(data, '$.role') = 'user'`, historyText, sid); err != nil {
				t.Fatal(err)
			}

			b := start(t, llm, withDataDir(dir))
			b.initialize()
			b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replayed := filter(b.untilIdle(sid), "user_message")
			if len(replayed) != 1 || replayed[0].MessageID != original {
				t.Fatalf("legacy replay: %+v", replayed)
			}
			assertJSON(t, replayed[0].Content, legacy)
			before := llm.requestCount()
			if multipart {
				llm.enqueue(reply("Summary of the ordinary answer."))
			}
			b.accepted(b.prompt(sid, "/compact"))
			updates := b.untilIdle(sid)
			wantCalls, wantUpdates := 0, 0
			if multipart {
				wantCalls, wantUpdates = 1, 2
			}
			if updates[len(updates)-1].StopReason != "end_turn" || len(filter(updates, "compaction_update")) != wantUpdates || llm.requestCount()-before != wantCalls {
				t.Fatalf("stored prompt classification: %v; new requests=%d", kinds(updates), llm.requestCount()-before)
			}
		})
	}
}
