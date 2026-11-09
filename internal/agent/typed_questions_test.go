package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

const typedQuestionArgs = `{"message":"Configure values","questions":[{"id":"count","title":"Count","question":"How many?","type":"integer"},{"id":"ratio","title":"Ratio","question":"What ratio?","type":"number"},{"id":"enabled","title":"Enabled","question":"Enable it?","type":"boolean"}]}`
const typedQuestionAnswers = `{"count":9223372036854775807,"ratio":1.2300e+2,"enabled":false}`

func TestElicitationTypedHandler(t *testing.T) {
	model := make(scriptedModel, 2)
	model.callTool("question", "ask_user", typedQuestionArgs)
	results := make(chan llm.Message, 1)
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		results <- req.Messages[len(req.Messages)-1]
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}
	c := startAgent(t, t.TempDir(), model, nil)
	c.formInitialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	c.await(c.prompt(sid, "ask"))
	q := c.question()
	var req acp.ElicitationRequest
	if err := json.Unmarshal(q.Params, &req); err != nil {
		t.Fatal(err)
	}
	for id, kind := range map[string]string{"count": "integer", "ratio": "number", "enabled": "boolean"} {
		if req.RequestedSchema.Properties[id].Type != kind {
			t.Fatalf("field %s: %+v", id, req.RequestedSchema.Properties[id])
		}
	}
	c.waitFor(sid, func(n notification) bool { return n.Update.State == "requires_action" })
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	c.waitFor(sid, func(n notification) bool { return n.Update.State == "requires_action" })
	select {
	case duplicate := <-c.elicitations:
		t.Fatalf("resume reissued form: %+v", duplicate)
	default:
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": map[string]any{"action": "accept", "content": json.RawMessage(typedQuestionAnswers)}})
	updates := c.untilIdle(sid)
	var completed bool
	for _, n := range updates {
		completed = completed || n.Update.Status == "completed"
	}
	result := <-results
	if !completed || result.ToolCallID != "question" || !strings.Contains(result.Content, `"count":9223372036854775807`) || !strings.Contains(result.Content, `"ratio":1.2300e+2`) || !strings.Contains(result.Content, `"enabled":false`) {
		t.Fatalf("completed=%v, tool result=%+v", completed, result)
	}
}
