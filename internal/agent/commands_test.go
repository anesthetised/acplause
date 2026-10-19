package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/media"
)

func TestCompactCommandDiscovery(t *testing.T) {
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.initialize()
	cwd := t.TempDir()
	var created struct {
		SessionID string           `json:"sessionId"`
		Commands  []map[string]any `json:"availableCommands"`
	}
	c.ok(acp.MethodSessionNew, map[string]any{"cwd": cwd}, &created)
	if len(created.Commands) != 1 || created.Commands[0]["name"] != "compact" || created.Commands[0]["description"] == "" || created.Commands[0]["input"] != nil {
		t.Fatalf("commands: %+v", created.Commands)
	}
	var resumed struct {
		Commands []map[string]any `json:"availableCommands"`
	}
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": created.SessionID, "cwd": cwd}, &resumed)
	if !reflect.DeepEqual(created.Commands, resumed.Commands) {
		t.Fatalf("resumed commands: %+v", resumed.Commands)
	}
}

func TestCompactCommandForcesSummaryBelowThreshold(t *testing.T) {
	calls := 0
	s, out := compactionSession(t, fakeModel(func(_ context.Context, req llm.Request, delta func(llm.Delta)) (*llm.Completion, error) {
		calls++
		if len(req.Tools) != 0 || req.Messages[0].Content != summaryInstruction || !strings.Contains(req.Messages[1].Content, "old answer") || strings.Contains(req.Messages[1].Content, "/compact") {
			t.Errorf("command made a normal request or summarized itself: %+v", req)
		}
		delta(llm.Delta{Kind: llm.DeltaText, Text: "summary must not stream"})
		return &llm.Completion{Content: "Task summary", FinishReason: llm.FinishStop, Usage: &llm.Usage{Cost: 0.01}}, nil
	}))
	s.deps.maxRounds = 1 // no answer request needs to be reserved for a command
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "old request"}, llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("old answer ", 30)})
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text(" /compact\n")})
	updates := out.untilIdle(t)
	idle := updates[len(updates)-1].(acp.StateUpdate)
	if idle.StopReason != acp.StopEndTurn || calls != 1 || s.cost != 0.01 {
		t.Fatalf("command: %+v; calls=%d cost=%f", idle, calls, s.cost)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) != 4 || s.history[1].Content != "Conversation summary:\nTask summary" || s.history[2].Content != " /compact\n" || !strings.Contains(s.history[3].Content, "compacted") {
		t.Fatalf("compacted history: %+v", s.history)
	}
	var statuses []acp.CompactionStatus
	for _, u := range updates {
		if c, ok := u.(acp.CompactionUpdate); ok {
			statuses = append(statuses, c.Status)
		}
		if c, ok := u.(acp.AgentMessageChunk); ok && strings.Contains(c.Content.Text, "must not stream") {
			t.Fatal("summary streamed into answer")
		}
	}
	if !reflect.DeepEqual(statuses, []acp.CompactionStatus{acp.CompactionInProgress, acp.CompactionCompleted}) {
		t.Fatalf("lifecycle: %v", statuses)
	}
}

func TestCompactCommandNoOp(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "no older history", true: "unknown window"}[unknown], func(t *testing.T) {
			s, out := compactionSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
				t.Error("no-op command reached the model")
				return &llm.Completion{FinishReason: llm.FinishStop}, nil
			}))
			if unknown {
				s.catalog = newCatalog([]llm.Model{{ID: "m"}})
				s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "earlier"}, llm.Message{Role: llm.RoleAssistant, Content: "answer"})
			}
			before := len(s.history)
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("/compact")})
			updates := out.untilIdle(t)
			if idle := updates[len(updates)-1].(acp.StateUpdate); idle.StopReason != acp.StopEndTurn {
				t.Fatalf("no-op: %+v", idle)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.history) != before+2 || s.history[before+1].Role != llm.RoleAssistant || s.history[before+1].Content == "" {
				t.Fatalf("no-op message: %+v", s.history)
			}
			for _, u := range updates {
				if _, ok := u.(acp.CompactionUpdate); ok {
					t.Fatal("no-op published compaction")
				}
			}
		})
	}
}

func TestCompactCommandRequiresSelectedModel(t *testing.T) {
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.initialize()
	sid := c.newSession(t.TempDir())
	result := c.await(c.prompt(sid, "/compact"))
	if result.Error == nil || !strings.Contains(result.Error.Message, "select a model") {
		t.Fatalf("unselected model: %+v", result)
	}
}

func TestCompactCommandRecognition(t *testing.T) {
	for _, tc := range []struct {
		wire string
		want bool
	}{
		{`[{"type":"text","text":" \n/compact\t","future":true}]`, true},
		{`[{"type":"text","text":"/compactness"}]`, false},
		{`[{"type":"text","text":"/compact explanation"}]`, false},
		{`[{"type":"resource","text":"/compact","resource":{"uri":"file:///cmd","text":"/compact"}}]`, false},
		{`[{"type":"_future","text":"/compact"}]`, false},
		{`[{"type":"text","text":"/compact"},{"type":"image","data":"","mimeType":"image/png"}]`, false},
		{`[{"type":"text","text":"/compact"},{"type":"text","text":""}]`, false},
		{`[]`, false},
	} {
		var blocks []acp.ContentBlock
		if err := json.Unmarshal([]byte(tc.wire), &blocks); err != nil {
			t.Fatal(err)
		}
		if got := isCompactCommand(blocks); got != tc.want {
			t.Errorf("recognize %s = %t", tc.wire, got)
		}
	}
}

func TestCompactCommandFailureKeepsHistory(t *testing.T) {
	for _, kind := range []string{"empty", "tools", "length", "error", "cancelled", "budget", "oversized", "nonreducing"} {
		t.Run(kind, func(t *testing.T) {
			var s *Session
			calls := 0
			s, out := compactionSession(t, fakeModel(func(ctx context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				calls++
				c := &llm.Completion{Content: "summary", FinishReason: llm.FinishStop}
				switch kind {
				case "empty":
					c.Content = " "
				case "tools":
					c.ToolCalls = callMessage("forbidden").ToolCalls
				case "length":
					c.FinishReason = llm.FinishLength
				case "error":
					return nil, errors.New("summary failed")
				case "cancelled":
					s.Cancel()
					<-ctx.Done()
					return nil, ctx.Err()
				case "oversized":
					c.Content = strings.Repeat("s", 16000)
				case "nonreducing":
					c.Content = strings.Repeat("s", 1000)
				}
				return c, nil
			}))
			s.deps.log = slog.New(slog.DiscardHandler)
			s.deps.maxRounds = 1
			old := "old request"
			if kind == "budget" {
				old = strings.Repeat("x", 9000)
			}
			answer := strings.Repeat("a", 8000)
			if kind == "nonreducing" {
				answer = "old answer"
			}
			s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: old}, llm.Message{Role: llm.RoleAssistant, Content: answer})
			before := slices.Clone(s.history)
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("/compact")})
			updates := out.untilIdle(t)
			idle := updates[len(updates)-1].(acp.StateUpdate)
			want := acp.CompactionFailed
			if kind == "cancelled" {
				want = acp.CompactionCancelled
				if idle.StopReason != acp.StopCancelled || idle.Error != nil {
					t.Fatalf("cancelled idle: %+v", idle)
				}
			} else if idle.StopReason != acp.StopError || idle.Error == nil {
				t.Fatalf("failure idle: %+v", idle)
			}
			if calls != 1 {
				t.Fatalf("requests=%d", calls)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if !reflect.DeepEqual(s.history[:len(before)], before) || len(s.history) != len(before)+1 || s.history[len(before)].Content != "/compact" {
				t.Fatal("failed command replaced history")
			}
			var statuses []acp.CompactionStatus
			for _, u := range updates {
				if c, ok := u.(acp.CompactionUpdate); ok {
					statuses = append(statuses, c.Status)
				}
			}
			if !reflect.DeepEqual(statuses, []acp.CompactionStatus{acp.CompactionInProgress, want}) {
				t.Fatalf("failure lifecycle: %v", statuses)
			}
		})
	}
}

func TestCompactCommandDescribesImagesAndKeepsToolPairs(t *testing.T) {
	img := testImage(t, 64, 48)
	encoded := strings.TrimPrefix(img.DataURL(), "data:image/png;base64,")
	s, out := imageSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		text := req.Messages[1].Content
		if req.Vision || imageCount(req.Messages) != 0 || strings.Contains(text, encoded[:32]) || !strings.Contains(text, "attached by the user") || !strings.Contains(text, "returned by this tool call") {
			t.Error("summary image descriptors missing or bytes leaked")
		}
		var data []llm.Message
		_, conversation, ok := strings.Cut(text, "\nConversation JSON:\n")
		if !ok || json.Unmarshal([]byte(conversation), &data) != nil {
			t.Fatal("invalid history JSON")
		}
		assertToolPairs(t, data)
		return &llm.Completion{Content: "Earlier images inspected.", FinishReason: llm.FinishStop}, nil
	}), 64000)
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "look", Images: []media.Image{img}}, callMessage("a", "b"), imageResult("a", "page", img), toolResult("b", "other result"), llm.Message{Role: llm.RoleAssistant, Content: "seen"})
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("/compact")})
	updates := out.untilIdle(t)
	if idle := updates[len(updates)-1].(acp.StateUpdate); idle.StopReason != acp.StopEndTurn {
		t.Fatalf("image command: %+v", idle)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if imageCount(s.history) != 0 || len(s.history) != 4 {
		t.Fatal("old image history was not summarized")
	}
}

func TestCompactCommandOverProtocolPersistsContextAndReplay(t *testing.T) {
	model := make(scriptedModel, 3)
	model.reply(strings.Repeat("old answer ", 30), llm.FinishStop, nil)
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		if len(req.Tools) != 0 || !strings.Contains(req.Messages[1].Content, "old answer") || strings.Contains(req.Messages[1].Content, "/compact") {
			t.Error("command did not make a summary-only request")
		}
		return &llm.Completion{Content: "Keep the task.", FinishReason: llm.FinishStop}, nil
	}
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	// Compaction remains an agent command in Plan mode, without tool authority.
	c.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": sid, "configId": "mode", "type": "id", "value": "plan"}, nil)
	c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("old request")}}, nil)
	c.untilIdle(sid)
	c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("/compact")}}, nil)
	updates := c.untilIdle(sid)
	if idle := lastIdle(updates); idle.Update.StopReason != "end_turn" || !hasKind(updates, "compaction_update") || hasKind(updates, "tool_call_update") {
		t.Fatalf("command: %+v", updates)
	}
	c.ok(acp.MethodSessionClose, map[string]any{"sessionId": sid}, nil)
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := c.untilIdle(sid)
	var kinds []string
	for _, n := range replay {
		kinds = append(kinds, n.Update.Kind)
	}
	if !slices.Equal(kinds, []string{"user_message", "agent_message", "user_message", "compaction_update", "agent_message", "state_update"}) {
		t.Fatalf("replay: %v", kinds)
	}
	var completed acp.CompactionUpdate
	if err := json.Unmarshal(replay[3].Update.Raw, &completed); err != nil || completed.Status != acp.CompactionCompleted || len(completed.Summary) != 1 || completed.Summary[0].Text != "Keep the task." {
		t.Fatalf("stored summary: %+v, %v", completed, err)
	}
	model <- func(_ context.Context, req llm.Request, delta func(llm.Delta)) (*llm.Completion, error) {
		if len(req.Messages) != 5 || req.Messages[1].Content != "Conversation summary:\nKeep the task." || req.Messages[2].Content != "/compact" || req.Messages[4].Content != "next" {
			t.Errorf("loaded context: %+v", req.Messages)
		}
		delta(llm.Delta{Kind: llm.DeltaText, Text: "continued"})
		return &llm.Completion{Content: "continued", FinishReason: llm.FinishStop}, nil
	}
	c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("next")}}, nil)
	if idle := lastIdle(c.untilIdle(sid)); idle.Update.StopReason != "end_turn" {
		t.Fatalf("continued: %+v", idle)
	}
}
