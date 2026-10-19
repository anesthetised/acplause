package e2e

import (
	"encoding/json"
	"testing"
)

func TestMalformedReasoningIdentityEndsTurnAndKeepsProcessAlive(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		func(s *stream) {
			fragment := map[string]any{"type": "reasoning.text", "index": []any{}, "text": "fragment"}
			s.delta(map[string]any{"reasoning_details": []any{fragment}})
			s.delta(map[string]any{"reasoning_details": []any{fragment}})
			s.done()
		},
		reply("still alive"),
	)
	a, sid, _ := started(t, llm)
	a.accepted(a.prompt(sid, "malformed response"))
	updates := a.untilIdle(sid)
	if len(filter(updates, "state_update")) != 2 {
		t.Fatalf("state updates: %v", kinds(updates))
	}
	idle := updates[len(updates)-1]
	if idle.StopReason != "error" || idle.Error == nil || idle.Error.Code != -32603 {
		t.Fatalf("idle: %+v", idle)
	}
	var data struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(idle.Error.Data, &data); err != nil || data.Kind != "protocol" {
		t.Fatalf("error data %s: %v", idle.Error.Data, err)
	}

	a.accepted(a.prompt(sid, "follow up"))
	later := a.untilIdle(sid)
	if later[len(later)-1].StopReason != "end_turn" || joinChunks(later, "agent_message_chunk").text != "still alive" {
		t.Fatalf("later turn: %v", kinds(later))
	}
}
