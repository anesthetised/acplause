package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// countingFinish ends a response like finish, but reports prompt tokens at
// bytesPerToken of the wire request, or none when it is zero.
func countingFinish(s *stream, reason string, bytesPerToken int) {
	usage := map[string]any{"completion_tokens": 5, "cost": 0.001}
	if bytesPerToken > 0 {
		wire, _ := json.Marshal(s.req)
		usage["prompt_tokens"] = len(wire) / bytesPerToken
	}
	s.data(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ""}, "finish_reason": reason}},
		"usage":   usage,
	})
	s.done()
}

func countedReply(text string, bytesPerToken int) script {
	return func(s *stream) {
		s.text(text)
		countingFinish(s, "stop", bytesPerToken)
	}
}

func countedListDir(bytesPerToken int) script {
	return func(s *stream) {
		s.toolCall(0, "call_list_dir", "list_dir", `{}`)
		countingFinish(s, "tool_calls", bytesPerToken)
	}
}

// TestUsageCalibrationAvoidsCompaction replays the compaction fixture: with
// no prompt token counts it compacts as before, and once usage reports a
// realistic ratio the same conversation fits without a summary request.
func TestUsageCalibrationAvoidsCompaction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		bytesPerToken int
		compacts      bool
	}{
		{"no usage", 0, true},
		{"realistic usage", 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, compactionModels)
			llm.enqueue(countedListDir(tc.bytesPerToken), countedReply(strings.Repeat("old answer ", 1200), tc.bytesPerToken))
			a, sid, _ := started(t, llm)
			a.accepted(a.prompt(sid, strings.Repeat("old request ", 650)))
			if updates := a.untilIdle(sid); updates[len(updates)-1].StopReason != "end_turn" {
				t.Fatalf("seed turn: %v", kinds(updates))
			}

			if tc.compacts {
				llm.enqueue(countedReply("Keep the plan.", 0))
			}
			llm.enqueue(countedReply("done", tc.bytesPerToken))
			a.accepted(a.prompt(sid, "continue"))
			updates := a.untilIdle(sid)
			compactions := filter(updates, "compaction_update")
			if updates[len(updates)-1].StopReason != "end_turn" || (len(compactions) > 0) != tc.compacts {
				t.Fatalf("compactions: %+v; turn: %v", compactions, kinds(updates))
			}
			last := llm.request(llm.requestCount() - 1)
			wire, _ := json.Marshal(last)
			if tc.compacts {
				assertBoundedChat(t, last)
			} else if last.MaxTokens != 4096 || uint64(len(wire)/tc.bytesPerToken)+last.MaxTokens > 32000 {
				t.Fatalf("request does not fit: %d tokens, %d output tokens", len(wire)/tc.bytesPerToken, last.MaxTokens)
			}
			if strings.Contains(string(wire), "old answer") == tc.compacts {
				t.Fatalf("answer request kept old history: %t", !tc.compacts)
			}
		})
	}
}
