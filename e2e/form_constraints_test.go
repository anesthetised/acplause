package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

const constrainedQuestionArgs = `{"message":"Configure","title":"Setup","description":"Release options","questions":[
 {"id":"name","title":"Name","question":"Name?","maxLength":8,"required":false,"default":"demo"},
 {"id":"level","title":"Level","question":"Level?","type":"integer","minimum":0,"maximum":5,"default":0},
 {"id":"mode","title":"Mode","question":"Mode?","choices":[{"value":"fast","title":"Fast path","description":"Quicker"},{"value":"safe","title":"Safe path"}]}]}`

func TestElicitationConstraintsAndTitledChoices(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, answers string
		valid         bool
		want          string
	}{
		{"optional omitted, zero default is an answer", `{"level":0,"mode":"safe"}`, true, `{"level":0,"mode":"safe"}`},
		{"all answered", `{"name":"ünïcode","level":5,"mode":"fast"}`, true, `{"level":5,"mode":"fast","name":"ünïcode"}`},
		{"title instead of value", `{"level":1,"mode":"Safe path"}`, false, ""},
		{"too long", `{"name":"123456789","level":1,"mode":"fast"}`, false, ""},
		{"out of range", `{"level":6,"mode":"fast"}`, false, ""},
		{"required omitted", `{"level":1}`, false, ""},
		{"null optional", `{"name":null,"level":1,"mode":"fast"}`, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := t.TempDir()
			llm := newMockLLM(t)
			llm.enqueue(callTool("ask_user", constrainedQuestionArgs), reply("Done."))
			a, sid, cwd := startedForms(t, llm, withDataDir(data))
			a.accepted(a.prompt(sid, "ask"))
			q := a.elicitationRequest()
			var req struct {
				RequestedSchema struct {
					Title, Description string
					Required           []string
					Properties         map[string]json.RawMessage
				}
			}
			if err := json.Unmarshal(q.Params, &req); err != nil {
				t.Fatal(err)
			}
			schema := req.RequestedSchema
			if schema.Title != "Setup" || schema.Description != "Release options" || len(schema.Required) != 2 {
				t.Fatalf("form metadata: %s", q.Params)
			}
			for id, want := range map[string]string{
				"name":  `"maxLength":8`,
				"level": `"default":0`,
				"mode":  `"oneOf":[{"const":"fast","title":"Fast path","description":"Quicker"},{"const":"safe","title":"Safe path"}]`,
			} {
				if !strings.Contains(string(schema.Properties[id]), want) {
					t.Fatalf("field %s lacks %s: %s", id, want, schema.Properties[id])
				}
			}
			a.answer(q, map[string]any{"action": "accept", "content": json.RawMessage(tt.answers)})
			calls := filter(a.untilIdle(sid), "tool_call_update")
			if len(calls) != 2 || (calls[1].Status == "completed") != tt.valid {
				t.Fatalf("constrained question result: %+v", calls)
			}
			last := llm.request(1).Messages
			content := last[len(last)-1]["content"].(string)
			if !tt.valid {
				if !strings.Contains(content, "Error:") {
					t.Fatalf("invalid answer did not reach the model as a failure: %s", content)
				}
				return
			}
			if content != tt.want || calls[1].text() != tt.want {
				t.Fatalf("model saw %s and client saw %s, want %s (defaults must not become answers)", content, calls[1].text(), tt.want)
			}
			a.stop()
			b := start(t, llm, withDataDir(data))
			b.initializeForms()
			b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replayed := filter(b.untilIdle(sid), "tool_call_update")
			if len(replayed) != 1 || replayed[0].Status != "completed" || replayed[0].text() != tt.want {
				t.Fatalf("restored result: %+v", replayed)
			}
			b.expectNoElicitation()
		})
	}
}

func TestElicitationTitledMultiselectAndAllOptional(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("ask_user", `{"message":"Pick","questions":[{"id":"parts","title":"Parts","question":"Which?","multiSelect":true,"required":false,"minItems":0,"maxItems":2,"default":[],"choices":[{"value":"a","title":"Alpha"},{"value":"b","title":"Beta"},{"value":"c","title":"Gamma"}]}]}`), reply("Done."))
	a, sid, _ := startedForms(t, llm)
	a.accepted(a.prompt(sid, "ask"))
	q := a.elicitationRequest()
	for _, want := range []string{`"anyOf":[{"const":"a","title":"Alpha"}`, `"minItems":0`, `"maxItems":2`, `"default":[]`} {
		if !strings.Contains(string(q.Params), want) {
			t.Fatalf("multiselect lacks %s: %s", want, q.Params)
		}
	}
	if strings.Contains(string(q.Params), `"required"`) {
		t.Fatalf("all-optional form advertised required fields: %s", q.Params)
	}
	a.answer(q, map[string]any{"action": "accept"})
	calls := filter(a.untilIdle(sid), "tool_call_update")
	if len(calls) != 2 || calls[1].Status != "completed" || calls[1].text() != `{}` {
		t.Fatalf("content-less accept: %+v", calls)
	}
}

func TestElicitationPatternRejected(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("ask_user", `{"message":"m","questions":[{"id":"v","title":"V","question":"V?","pattern":"^a+$"}]}`), reply("Done."))
	a, sid, _ := startedForms(t, llm)
	a.accepted(a.prompt(sid, "ask"))
	calls := filter(a.untilIdle(sid), "tool_call_update")
	a.expectNoElicitation()
	last := llm.request(1).Messages
	if len(calls) == 0 || calls[len(calls)-1].Status != "failed" || !strings.Contains(last[len(last)-1]["content"].(string), "pattern is not supported") {
		t.Fatalf("pattern was not rejected as a model-facing tool failure: %+v", calls)
	}
}
