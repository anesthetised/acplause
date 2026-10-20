package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// turnBudgetResponse supplies exact accounting without the default usage footer.
func turnBudgetResponse(text, usage string, calls bool) script {
	return func(s *stream) {
		s.text(text)
		reason := "stop"
		if calls {
			s.toolCall(0, "budget-write", "write_file", `{"path":"changed","content":"bad"}`)
			s.toolCall(1, "budget-bash", "bash", `{"command":"touch changed"}`)
			reason = "tool_calls"
		}
		accounting := ""
		if usage != "" {
			accounting = `,"usage":` + usage
		}
		s.raw(`data: {"choices":[{"delta":{},"finish_reason":"` + reason + `"}]` + accounting + "}\n\n")
		s.done()
	}
}

func TestTurnBudgetAccountingStopsToolsAndPersistsReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tokens, cost, usage, reason string
		calls                             bool
	}{
		{"token exact", "15", "0", `{"prompt_tokens":10,"completion_tokens":5,"cost":0.001,"completion_tokens_details":{"reasoning_tokens":3}}`, "token budget reached", true},
		{"token above", "15", "0", `{"prompt_tokens":10,"completion_tokens":6}`, "token budget reached", true},
		{"USD exact", "0", "0.001", `{"cost":0.001}`, "USD budget reached", true},
		{"joint USD first", "30", "0.001", `{"prompt_tokens":10,"completion_tokens":5,"cost":0.001}`, "USD budget reached", true},
		{"null output", "15", "0", `{"prompt_tokens":10,"completion_tokens":null}`, "accounting unavailable", true},
		{"missing USD", "0", "1", `{"prompt_tokens":1,"completion_tokens":1}`, "accounting unavailable", true},
		{"null USD", "0", "1", `{"cost":null}`, "accounting unavailable", true},
		{"negative USD", "0", "1", `{"cost":-1}`, "accounting unavailable", true},
		{"token overflow", "18446744073709551615", "0", `{"prompt_tokens":18446744073709551615,"completion_tokens":1}`, "accounting unavailable", true},
		{"final answer exact", "15", "0", `{"prompt_tokens":10,"completion_tokens":5}`, "token budget reached", false},
		{"below cap", "16", "0", `{"prompt_tokens":10,"completion_tokens":5}`, "", false},
		{"zero/free", "1", "0.001", `{"prompt_tokens":0,"completion_tokens":0,"cost":0}`, "", false},
		{"token only missing USD", "2", "0", `{"prompt_tokens":1,"completion_tokens":0}`, "", false},
		{"cost only missing tokens", "0", "1", `{"cost":0}`, "", false},
		{"all disabled", "0", "0", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := t.TempDir()
			llm := newMockLLM(t, compactionModels)
			llm.enqueue(turnBudgetResponse("Retained budget answer.", tc.usage, tc.calls))
			a, sid, cwd := started(t, llm, askingUser, withDataDir(data), withEnv("ACPLAUSE_MAX_TURN_TOKENS="+tc.tokens, "ACPLAUSE_MAX_TURN_COST_USD="+tc.cost))
			a.accepted(a.prompt(sid, "work"))
			updates := a.untilIdle(sid)
			idle := updates[len(updates)-1]
			if tc.reason == "" {
				if idle.StopReason != "end_turn" || idle.Error != nil {
					t.Fatalf("allowed accounting stopped work: %v", kinds(updates))
				}
			} else if idle.StopReason != "error" || idle.Error == nil || idle.Error.Code != -32603 || !strings.Contains(idle.Error.Message, tc.reason) {
				t.Fatalf("budget stop: %+v", idle)
			}
			if llm.requestCount() != 1 || joinChunks(updates, "agent_message_chunk").text != "Retained budget answer." {
				t.Fatal("budget discarded output or started another request")
			}
			calls := filter(updates, "tool_call_update")
			if tc.calls {
				if len(calls) != 2 || calls[0].Status != "failed" || calls[1].Status != "failed" {
					t.Fatalf("budgeted sibling calls: %+v", calls)
				}
				for _, call := range calls {
					if !strings.Contains(call.text(), tc.reason) {
						t.Fatalf("wrong failed result: %s", call.text())
					}
				}
			}
			for _, state := range filter(updates, "state_update") {
				if state.State == "requires_action" {
					t.Fatal("budget started permission waiting")
				}
			}
			llm.enqueue(turnBudgetResponse("Fresh turn.", `{"prompt_tokens":0,"completion_tokens":0,"cost":0}`, false))
			a.accepted(a.prompt(sid, "continue"))
			next := a.untilIdle(sid)
			if idle := next[len(next)-1]; idle.StopReason != "end_turn" {
				t.Fatalf("budget reset failed: %+v", idle)
			}
			assertBoundedChat(t, llm.request(1))
			if _, err := os.Stat(filepath.Join(cwd, "changed")); !os.IsNotExist(err) {
				t.Fatalf("budgeted tool wrote: %v", err)
			}
			if tc.name == "token exact" || tc.name == "USD exact" {
				a.stop()
				b := start(t, llm, askingUser, withDataDir(data))
				b.initialize()
				b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
				replay := b.untilIdle(sid)
				stored := filter(replay, "tool_call_update")
				if len(stored) != 2 || stored[0].Status != "failed" || stored[1].Status != "failed" || len(filter(replay, "agent_message")) != 2 || llm.requestCount() != 2 {
					t.Fatalf("restart lost outcomes or replayed work: %v", kinds(replay))
				}
			}
		})
	}
}

func TestTurnBudgetCompactCrossingPreservesHistoryAndChargesUsage(t *testing.T) {
	t.Parallel()
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "manual"}[manual], func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, compactionModels)
			zero := `{"prompt_tokens":0,"completion_tokens":0,"cost":0}`
			oldAnswer := strings.Repeat("old answer ", 1200)
			llm.enqueue(turnBudgetResponse(oldAnswer, zero, false))
			a, sid, cwd := started(t, llm, withEnv("ACPLAUSE_MAX_TURN_TOKENS=4"))
			a.accepted(a.prompt(sid, strings.Repeat("old request ", 650)))
			a.untilIdle(sid)
			llm.enqueue(turnBudgetResponse("Summary must not install.", `{"prompt_tokens":3,"completion_tokens":1,"cost":0.25}`, false))
			prompt := "continue"
			if manual {
				prompt = "/compact"
			}
			a.accepted(a.prompt(sid, prompt))
			updates := a.untilIdle(sid)
			c := filter(updates, "compaction_update")
			usage := filter(updates, "usage_update")
			if idle := updates[len(updates)-1]; idle.StopReason != "error" || len(c) != 2 || c[1].Status != "failed" || !strings.Contains(c[1].CompactionError, "token budget reached") || llm.requestCount() != 2 || len(usage) != 1 || usage[0].Cost.Amount != 0.25 {
				t.Fatalf("compaction budget: %v", kinds(updates))
			}
			llm.enqueue(turnBudgetResponse("Recovered summary.", zero, false))
			a.accepted(a.prompt(sid, "/compact"))
			next := a.untilIdle(sid)
			if idle := next[len(next)-1]; idle.StopReason != "end_turn" || llm.requestCount() != 3 {
				t.Fatalf("compaction allowance did not reset: %v", kinds(next))
			}
			if input := llm.request(2).Messages[1]["content"].(string); !strings.Contains(input, "old answer") || strings.Contains(input, "Summary must not install.") {
				t.Fatal("failed compaction discarded original history")
			}
			a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := a.untilIdle(sid)
			answers := filter(replay, "agent_message")
			if len(answers) != 2 || answers[0].text() != oldAnswer {
				t.Fatalf("budget failure damaged full replay: %v", kinds(replay))
			}
		})
	}
}
