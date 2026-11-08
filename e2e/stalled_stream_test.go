package e2e

import (
	"encoding/json"
	"testing"
)

func TestStalledStreamEndsTheTurnWithATimeoutError(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(func(s *stream) {
		s.text("partial")
		<-s.r.Context().Done() // silence until the agent gives up on the connection
	})
	a, sid, _ := started(t, llm, withEnv("ACPLAUSE_IDLE_TIMEOUT=3s"))
	a.accepted(a.prompt(sid, "hi"))
	updates := a.untilIdle(sid)

	idles := 0
	for _, u := range filter(updates, "state_update") {
		if u.State == "idle" {
			idles++
		}
	}
	idle := updates[len(updates)-1]
	var data struct {
		Kind string `json:"kind"`
	}
	if idles != 1 || idle.State != "idle" || idle.StopReason != "error" || idle.Error == nil || json.Unmarshal(idle.Error.Data, &data) != nil || data.Kind != "timeout" {
		t.Fatalf("stalled stream: %v, error %+v", kinds(updates), idle.Error)
	}
	if llm.requestCount() != 1 {
		t.Fatalf("a started stream was replayed: %d requests", llm.requestCount())
	}
}
