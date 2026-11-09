package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

const questionArgs = `{"message":"Choose <&>","questions":[{"id":"choice","title":"Choice","question":"Which?","options":["a","b"]},{"id":"detail","title":"Detail","question":"Why?"},{"id":"last","title":"Last","question":"Anything else?"}]}`

// clientElicitation is captured by the pipe client without answering it.
type clientElicitation struct {
	ID     int
	Params json.RawMessage
}

func (c *acpClient) formInitialize() {
	c.t.Helper()
	c.ok(acp.MethodInitialize, map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "forms", "version": "1"}, "capabilities": map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}}, nil)
}

func (c *acpClient) question() clientElicitation {
	c.t.Helper()
	select {
	case q := <-c.elicitations:
		return q
	case <-time.After(10 * time.Second):
		c.t.Fatal("no form request")
		return clientElicitation{}
	}
}

func TestElicitationLifecycle(t *testing.T) {
	for _, action := range []string{"accept", "decline", "cancel", "_future", "rpc error"} {
		t.Run(action, func(t *testing.T) {
			model := make(scriptedModel, 2)
			model.callTool("question", "ask_user", questionArgs)
			results := make(chan llm.Message, 1)
			model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				results <- req.Messages[len(req.Messages)-1]
				return &llm.Completion{FinishReason: llm.FinishStop}, nil
			}
			c := startAgent(t, t.TempDir(), model, nil)
			c.formInitialize()
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			c.await(c.prompt(sid, "ask"))
			q := c.question()
			var req acp.ElicitationRequest
			if err := json.Unmarshal(q.Params, &req); err != nil {
				t.Fatal(err)
			}
			if req.SessionID != sid || req.ToolCallID == "" || req.Mode != "form" || len(req.RequestedSchema.Properties) != 3 {
				t.Fatalf("form: %+v", req)
			}
			response := map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": map[string]any{"action": action, "content": map[string]string{"choice": "a", "detail": "<&>", "last": ""}}}
			if action == "rpc error" {
				delete(response, "result")
				response["error"] = map[string]any{"code": -32603, "message": "unavailable"}
			}
			c.write(response)
			updates := c.untilIdle(sid)
			var states []string
			var statuses []string
			for _, u := range updates {
				if u.Update.Kind == "state_update" {
					states = append(states, u.Update.State)
				}
				if u.Update.Kind == "tool_call_update" {
					statuses = append(statuses, u.Update.Status)
				}
			}
			if !slices.Equal(states, []string{"running", "requires_action", "running", "idle"}) {
				t.Fatalf("states: %v", states)
			}
			want := "failed"
			if action == "accept" {
				want = "completed"
			}
			if !slices.Equal(statuses, []string{"pending", want}) {
				t.Fatalf("statuses: %v", statuses)
			}
			result := <-results
			if result.ToolCallID != "question" || (action == "accept") != strings.Contains(result.Content, `<&>`) {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

func TestElicitationStopsWithSession(t *testing.T) {
	for _, stop := range []string{"cancel", "close", "delete", "lease loss", "answer race"} {
		t.Run(stop, func(t *testing.T) {
			model := make(scriptedModel, 2)
			model.callTool("question", "ask_user", typedQuestionArgs)
			// If a simultaneous answer wins, it still cannot start another model round
			// after cancellation is observed by the worker.
			c := startAgent(t, t.TempDir(), model, nil)
			c.formInitialize()
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			c.await(c.prompt(sid, "ask"))
			q := c.question()
			c.waitFor(sid, func(n notification) bool { return n.Update.State == "requires_action" })
			switch stop {
			case "close":
				c.ok(acp.MethodSessionClose, map[string]any{"sessionId": sid}, nil)
			case "delete":
				c.ok(acp.MethodSessionDelete, map[string]any{"sessionId": sid}, nil)
			case "lease loss":
				c.agent.drop(sid)
			case "answer race":
				// Cancel first, then answer while the outbound waiter is being removed.
				c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
				c.write(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": map[string]any{"action": "accept", "content": json.RawMessage(typedQuestionAnswers)}})
			default:
				c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
			}
			updates := c.untilIdle(sid)
			if lastIdle(updates).Update.StopReason != "cancelled" {
				t.Fatalf("idle: %+v", updates)
			}
			for _, u := range updates {
				if u.Update.State == "running" {
					t.Fatal("cancelled form resumed work")
				}
			}
			c.agent.mu.Lock()
			s := c.agent.sessions[sid]
			c.agent.mu.Unlock()
			if s != nil {
				s.mu.Lock()
				last := s.history[len(s.history)-1]
				s.mu.Unlock()
				if last.ToolCallID != "question" || !strings.Contains(last.Content, "Cancelled") {
					t.Fatalf("history: %+v", last)
				}
			}
			// Late responses are ignored; initialize is a read-loop barrier.
			c.write(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": map[string]any{"action": "accept", "content": json.RawMessage(typedQuestionAnswers)}})
			c.formInitialize()
		})
	}
}

func TestElicitationResumeKeepsSingleRequest(t *testing.T) {
	model := make(scriptedModel, 2)
	model.callTool("question", "ask_user", questionArgs)
	model.reply("done", llm.FinishStop, nil)
	c := startAgent(t, t.TempDir(), model, nil)
	c.formInitialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	c.await(c.prompt(sid, "ask"))
	q := c.question()
	c.waitFor(sid, func(n notification) bool { return n.Update.State == "requires_action" })
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := c.waitFor(sid, func(n notification) bool { return n.Update.Kind == "tool_call_update" })
	snapshot := c.waitFor(sid, func(n notification) bool { return n.Update.Kind == "state_update" })
	if replay.Update.Status != "pending" || snapshot.Update.State != "requires_action" {
		t.Fatalf("resume: %+v %+v", replay, snapshot)
	}
	select {
	case duplicate := <-c.elicitations:
		t.Fatalf("duplicate: %+v", duplicate)
	default:
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": map[string]any{"action": "accept", "content": map[string]string{"choice": "b", "detail": "answer original", "last": ""}}})
	c.untilIdle(sid)
}

func TestElicitationCapabilityControlsCatalog(t *testing.T) {
	for _, forms := range []bool{false, true} {
		t.Run(fmt.Sprint(forms), func(t *testing.T) {
			model := make(scriptedModel, 2)
			model.callTool("question", "ask_user", questionArgs)
			model.reply("done", llm.FinishStop, nil)
			c := startAgent(t, t.TempDir(), model, nil)
			if forms {
				c.formInitialize()
			} else {
				c.initialize()
			}
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			c.agent.mu.Lock()
			s := c.agent.sessions[sid]
			c.agent.mu.Unlock()
			catalog := s.request()
			index := slices.IndexFunc(catalog.Tools, func(t llm.Tool) bool { return t.Function.Name == "ask_user" })
			if (index >= 0) != forms {
				t.Fatalf("ask_user tool index=%d, supported=%v", index, forms)
			}
			if forms {
				description := catalog.Tools[index].Function.Description
				for _, instruction := range []string{"Never request secrets or credentials", "passwords", "API keys", "outside this conversation", "Do not ask them to paste secrets into chat"} {
					if !strings.Contains(description, instruction) {
						t.Errorf("missing form safety instruction %q", instruction)
					}
				}
			}
			if !forms {
				c.await(c.prompt(sid, "ask"))
				updates := c.untilIdle(sid)
				if !slices.ContainsFunc(updates, func(n notification) bool { return n.Update.Status == "failed" }) {
					t.Fatalf("unavailable tool did not fail: %+v", updates)
				}
			}
		})
	}
}

func TestElicitationRecoveryAfterProcessLoss(t *testing.T) {
	assistant, _ := jsonrpc.Marshal(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "question", Function: llm.FunctionCall{Name: "ask_user", Arguments: questionArgs}}}})
	pending, err := encodeEntry(acp.ToolCallUpdate{ToolCallID: "visible-question", Name: "ask_user", Status: acp.ToolPending})
	if err != nil {
		t.Fatal(err)
	}
	s, err := restoreSession(context.Background(), deps{}, newCatalog(nil), &store.Loaded{Session: store.Session{ID: "restored", Cwd: t.TempDir(), State: "requires_action"}, History: [][]byte{assistant}, Transcript: [][]byte{pending}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.state.State != acp.StateIdle || s.supportsForms() {
		t.Fatalf("connection state persisted: %+v", s.state)
	}
	call := s.transcript[0].(acp.ToolCallUpdate)
	last := s.history[len(s.history)-1]
	if call.Status != acp.ToolFailed || last.ToolCallID != "question" || last.Content != interrupted {
		t.Fatalf("recovery: %+v %+v", call, last)
	}
	s.repair()
	if len(s.history) != 3 {
		t.Fatal("duplicate tool result after recovery")
	}
}
