package agent

import (
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

func TestCompactClassifiesStoredPrompts(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "ordinary multipart"}[multipart], func(t *testing.T) {
			model := make(scriptedModel, 2)
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			cwd := t.TempDir()
			sid := c.newSession(cwd)
			c.selectModel(sid, "m")
			prompt := []acp.ContentBlock{acp.Text("/compact")}
			if multipart {
				prompt = append(prompt, acp.Text(""))
				model.reply(strings.Repeat("ordinary answer ", 100), llm.FinishStop, nil)
			}
			c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": prompt}, nil)
			c.untilIdle(sid)
			c.ok(acp.MethodSessionClose, map[string]any{"sessionId": sid}, nil)
			c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd}, nil)
			c.untilIdle(sid)
			if multipart {
				model.reply("Summary of the ordinary answer.", llm.FinishStop, nil)
			}
			c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("/compact")}}, nil)
			updates := c.untilIdle(sid)
			if idle := lastIdle(updates); idle.Update.StopReason != "end_turn" || hasKind(updates, "compaction_update") != multipart || len(model) != 0 {
				t.Fatalf("stored prompt classification: idle=%+v compacted=%t unused replies=%d", idle, hasKind(updates, "compaction_update"), len(model))
			}
		})
	}
}
