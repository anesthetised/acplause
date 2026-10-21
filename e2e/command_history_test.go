package e2e

import (
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
			blocks := []any{map[string]any{"type": "text", "text": "/compact"}}
			if multipart {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
				llm.enqueue(reply(strings.Repeat("ordinary answer ", 100)))
			}
			a.accepted(a.request("session/prompt", imageParams(sid, blocks)))
			a.untilIdle(sid)
			a.stop()
			b := start(t, llm, withDataDir(dir))
			b.initialize()
			b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, nil)
			b.untilIdle(sid)
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
