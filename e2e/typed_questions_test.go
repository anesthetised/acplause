package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

const typedQuestionArgs = `{"message":"Configure values","questions":[{"id":"count","title":"Count","question":"How many?","type":"integer"},{"id":"ratio","title":"Ratio","question":"What ratio?","type":"number"},{"id":"enabled","title":"Enabled","question":"Enable it?","type":"boolean"}]}`
const typedQuestionAnswers = `{"count":9223372036854775807,"ratio":1.2300e+2,"enabled":false}`

func assertTypedAnswers(t *testing.T, text string) {
	t.Helper()
	var answers map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &answers); err != nil {
		t.Fatal(err)
	}
	if len(answers) != 3 || string(answers["count"]) != "9223372036854775807" || string(answers["ratio"]) != "1.2300e+2" || string(answers["enabled"]) != "false" {
		t.Fatalf("lost typed answers: %s", text)
	}
}

func TestElicitationTypedAnswers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, answers string
		valid         bool
	}{
		{"accepted and restored", typedQuestionAnswers, true},
		{"quoted integer", strings.Replace(typedQuestionAnswers, `9223372036854775807`, `"1"`, 1), false},
		{"overflow", strings.Replace(typedQuestionAnswers, `9223372036854775807`, `9223372036854775808`, 1), false},
		{"null boolean", strings.Replace(typedQuestionAnswers, `false`, `null`, 1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := t.TempDir()
			llm := newMockLLM(t)
			llm.enqueue(callTool("ask_user", typedQuestionArgs), reply("Done."))
			a, sid, cwd := startedForms(t, llm, withDataDir(data))
			a.accepted(a.prompt(sid, "ask for typed values"))
			q := a.elicitationRequest()
			var req struct {
				RequestedSchema struct {
					Properties map[string]struct{ Type string }
				}
			}
			if err := json.Unmarshal(q.Params, &req); err != nil {
				t.Fatal(err)
			}
			for id, kind := range map[string]string{"count": "integer", "ratio": "number", "enabled": "boolean"} {
				if req.RequestedSchema.Properties[id].Type != kind {
					t.Fatalf("wrong %s schema: %s", id, q.Params)
				}
			}
			a.answer(q, map[string]any{"action": "accept", "content": json.RawMessage(tt.answers)})
			calls := filter(a.untilIdle(sid), "tool_call_update")
			if len(calls) != 2 || (calls[1].Status == "completed") != tt.valid {
				t.Fatalf("typed question result: %+v", calls)
			}
			request := llm.request(1)
			last := request.Messages[len(request.Messages)-1]
			if last["role"] != "tool" {
				t.Fatalf("missing tool result: %+v", last)
			}
			if !tt.valid {
				if !strings.Contains(last["content"].(string), "Error:") {
					t.Fatalf("invalid answer did not reach model as failure: %+v", last)
				}
				return
			}
			assertTypedAnswers(t, last["content"].(string))
			assertTypedAnswers(t, calls[1].text())
			a.stop()
			b := start(t, llm, withDataDir(data))
			b.initializeForms()
			b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replayed := filter(b.untilIdle(sid), "tool_call_update")
			if len(replayed) != 1 || replayed[0].Status != "completed" {
				t.Fatalf("restored typed result: %+v", replayed)
			}
			assertTypedAnswers(t, replayed[0].text())
			b.expectNoElicitation()
			llm.enqueue(reply("Continue."))
			b.accepted(b.prompt(sid, "continue"))
			b.untilIdle(sid)
			var found bool
			for _, m := range llm.request(2).Messages {
				if m["role"] == "tool" {
					assertTypedAnswers(t, m["content"].(string))
					found = true
				}
			}
			if !found {
				t.Fatal("restored model history lost typed tool result")
			}
		})
	}
}
