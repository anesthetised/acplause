package e2e

import "testing"

func TestUsageAccountingCompatibilityWithoutTurnLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, usage string
		wantUpdate  bool
		tokens      uint64
		cost        float64
	}{
		{"absent", "", false, 0, 0},
		{"null", `,"usage":null`, false, 0, 0},
		{"empty", `,"usage":{}`, false, 0, 0},
		{"only cost", `,"usage":{"cost":0.5}`, false, 0, 0},
		{"one count", `,"usage":{"prompt_tokens":10,"cost":0.5}`, false, 0, 0},
		{"free", `,"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0}`, true, 15, 0},
		{"zero", `,"usage":{"prompt_tokens":0,"completion_tokens":0,"cost":0}`, true, 0, 0},
		{"missing cost", `,"usage":{"prompt_tokens":10,"completion_tokens":5}`, true, 15, 0},
		{"null cost", `,"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":null}`, true, 15, 0},
		{"subsets", `,"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":999,"cost":0.25,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2},"future":true}`, true, 15, 0.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(func(s *stream) {
				s.raw(`data: {"choices":[{"delta":{"content":"Done."},"finish_reason":"stop"}]` + tc.usage + "}\n\n")
				s.done()
			})
			a, sid, _ := started(t, llm)
			a.accepted(a.prompt(sid, "continue without usage limits"))
			updates := a.untilIdle(sid)
			usage := filter(updates, "usage_update")
			if idle := updates[len(updates)-1]; idle.StopReason != "end_turn" || joinChunks(updates, "agent_message_chunk").text != "Done." || (len(usage) == 1) != tc.wantUpdate {
				t.Fatalf("usage changed default turn behavior: %v", kinds(updates))
			}
			if tc.wantUpdate && (usage[0].Used != tc.tokens || usage[0].Cost == nil || usage[0].Cost.Amount != tc.cost) {
				t.Fatalf("legacy usage projection changed: %+v", usage[0])
			}
		})
	}
}
