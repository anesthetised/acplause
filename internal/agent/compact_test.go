package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

func compactionSession(t *testing.T, model Model) (*Session, recorder) {
	t.Helper()
	s, out := testSession(t, model)
	s.catalog = newCatalog([]llm.Model{{ID: "m", ContextLength: 16000}})
	return s, out
}

func callMessage(ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}})
	}
	return m
}

func assertToolPairs(t *testing.T, messages []llm.Message) {
	t.Helper()
	pending := map[string]bool{}
	for _, m := range messages {
		if m.Role == llm.RoleTool {
			if !pending[m.ToolCallID] {
				t.Fatalf("orphan result %q", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			continue
		}
		if len(pending) > 0 {
			t.Fatalf("unanswered calls: %v", pending)
		}
		for _, c := range m.ToolCalls {
			pending[c.ID] = true
		}
	}
	if len(pending) > 0 {
		t.Fatalf("unanswered calls at end: %v", pending)
	}
}

func TestPrepareTrimsOldToolOutputFirst(t *testing.T) {
	s, out := compactionSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		t.Fatal("trimming should avoid a summary request")
		return nil, nil
	}))
	old := strings.Repeat("old ", 2000)
	latest := strings.Repeat("new ", 750)
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "read"}, callMessage("a"), toolResult("a", old), callMessage("b"), toolResult("b", latest))
	remaining := 3
	req, err := s.prepareRequest(context.Background(), &remaining)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[3].Content) > 2048 || !strings.Contains(req.Messages[3].Content, "omitted") || req.Messages[5].Content != latest {
		t.Fatal("did not trim the oldest result first")
	}
	if requestSize(req.Request) > 12000 || req.MaxTokens != 4000 || remaining != 3 || len(out) != 0 {
		t.Fatalf("unexpected budget or summary: size=%d, remaining=%d", requestSize(req.Request), remaining)
	}
	assertToolPairs(t, req.Messages)
	if !reflect.DeepEqual(s.history, req.Messages) {
		t.Fatal("trimming was not retained")
	}
}

func TestCompactionKeepsLatestPromptAndToolExchange(t *testing.T) {
	calls := 0
	s, out := compactionSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		calls++
		if requestSize(req) > 12000 || len(req.Tools) != 0 || req.MaxTokens == 0 {
			t.Fatalf("unbounded summary request: size=%d", requestSize(req))
		}
		assertToolPairs(t, req.Messages)
		for _, m := range req.Messages {
			if m.Reasoning != "" || len(m.ReasoningDetails) != 0 || strings.Contains(m.Content, "private") {
				t.Fatal("hidden reasoning supplied to displayable summary")
			}
		}
		return &llm.Completion{Content: "Remember the task and file changes.", FinishReason: llm.FinishStop}, nil
	}))
	recent := callMessage("x", "y")
	recent.Reasoning = "keep this for the next tool round"
	s.history = append(s.history,
		llm.Message{Role: llm.RoleUser, Content: strings.Repeat("old request ", 600)},
		llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("old answer ", 600), Reasoning: "private"},
		llm.Message{Role: llm.RoleUser, Content: "current request"}, recent, toolResult("x", "one"), toolResult("y", "two"))
	keep := append([]llm.Message(nil), s.history[3:]...)
	remaining := 10
	req, err := s.prepareRequest(context.Background(), &remaining)
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 || remaining != 10-calls || requestSize(req.Request) > 12000 {
		t.Fatalf("chunked compaction: calls=%d remaining=%d size=%d", calls, remaining, requestSize(req.Request))
	}
	if req.Messages[1].Role != llm.RoleAssistant || !strings.Contains(req.Messages[1].Content, "Remember") || !reflect.DeepEqual(req.Messages[2:], keep) {
		t.Fatalf("retained history: %+v", req.Messages)
	}
	assertToolPairs(t, req.Messages)
	start := (<-out).(acp.CompactionUpdate)
	end := (<-out).(acp.CompactionUpdate)
	if start.Status != acp.CompactionInProgress || end.Status != acp.CompactionCompleted || start.CompactionID != end.CompactionID || len(end.Summary) != 1 {
		t.Fatalf("compaction lifecycle: %+v, %+v", start, end)
	}
	if len(s.transcript) != 1 || !reflect.DeepEqual(s.transcript[0], end) {
		t.Fatal("replay did not keep the final snapshot in place")
	}
}

func TestCompactionWithUnlimitedRequests(t *testing.T) {
	s, out := compactionSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		return &llm.Completion{Content: "Remember the task.", FinishReason: llm.FinishStop}, nil
	}))
	s.history = append(s.history,
		llm.Message{Role: llm.RoleUser, Content: strings.Repeat("old request ", 600)},
		llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("old answer ", 600)},
		llm.Message{Role: llm.RoleUser, Content: "continue"})
	req, err := s.prepareRequest(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if requestSize(req.Request) > 12000 || req.Messages[2].Content != "continue" {
		t.Fatalf("compacted request: %+v", req)
	}
	start, end := (<-out).(acp.CompactionUpdate), (<-out).(acp.CompactionUpdate)
	if start.Status != acp.CompactionInProgress || end.Status != acp.CompactionCompleted {
		t.Fatalf("compaction: %+v, %+v", start, end)
	}
}

func TestCompactionFailurePreservesHistory(t *testing.T) {
	for _, kind := range []string{"empty", "tool", "length", "oversized", "error", "cancelled", "limit"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, out := compactionSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
				c := &llm.Completion{Content: "summary", FinishReason: llm.FinishStop}
				switch kind {
				case "empty":
					c.Content = " "
				case "tool":
					c.ToolCalls = callMessage("bad").ToolCalls
				case "length":
					c.FinishReason = llm.FinishLength
				case "oversized":
					c.Content = strings.Repeat("x", 16000)
				case "error":
					return nil, errors.New("summary failed")
				case "cancelled":
					cancel()
				}
				return c, nil
			}))
			s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: strings.Repeat("x", 9000)}, llm.Message{Role: llm.RoleAssistant, Content: "answer"}, llm.Message{Role: llm.RoleUser, Content: "continue"})
			before := append([]llm.Message(nil), s.history...)
			// Include tool definitions in the trigger without bloating the summary.
			s.history[1].Content += strings.Repeat("y", 2000)
			before[1] = s.history[1]
			remaining := 5
			if kind == "limit" {
				remaining = 1
			}
			_, err := s.prepareRequest(ctx, &remaining)
			if err == nil || !reflect.DeepEqual(s.history, before) {
				t.Fatalf("failed compaction changed history: err=%v", err)
			}
			start := (<-out).(acp.CompactionUpdate)
			end := (<-out).(acp.CompactionUpdate)
			want := acp.CompactionFailed
			if kind == "cancelled" {
				want = acp.CompactionCancelled
			}
			if start.CompactionID != end.CompactionID || end.Status != want || len(end.Summary) != 0 {
				t.Fatalf("failed lifecycle: %+v", end)
			}
		})
	}
}

func TestCompactionRejectsOversizedPrompt(t *testing.T) {
	s, _ := compactionSession(t, nil)
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: strings.Repeat("x", 16000)})
	remaining := 3
	if _, err := s.prepareRequest(context.Background(), &remaining); err == nil {
		t.Fatal("oversized prompt accepted")
	}
	// Without a published window, do not guess one or issue summary requests.
	s.catalog = newCatalog([]llm.Model{{ID: "m"}})
	req, err := s.prepareRequest(context.Background(), &remaining)
	if err != nil || req.MaxTokens != 0 || len(req.Messages[1].Content) != 16000 {
		t.Fatalf("unknown window changed behavior: %v", err)
	}
}

func TestCompactionUsesNewConfigAfterSummary(t *testing.T) {
	var s *Session
	s, _ = compactionSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		if err := s.SetConfigOption(configModel, acp.ConfigValueID, []byte(`"other"`)); err != nil {
			t.Fatal(err)
		}
		if err := s.Resume(context.Background(), []string{t.TempDir()}, nil, false); err != nil {
			t.Fatal(err)
		}
		return &llm.Completion{Content: "summary", FinishReason: llm.FinishStop}, nil
	}))
	s.catalog = newCatalog([]llm.Model{{ID: "m", ContextLength: 16000}, {ID: "other", ContextLength: 32000}})
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: strings.Repeat("x", 11000)}, llm.Message{Role: llm.RoleAssistant, Content: "answer"}, llm.Message{Role: llm.RoleUser, Content: "continue"})
	remaining := 5
	req, err := s.prepareRequest(context.Background(), &remaining)
	if err != nil || req.Model != "other" || req.MaxTokens != 8000 || !strings.Contains(req.Messages[0].Content, "Additional directories:") {
		t.Fatalf("stale model/system prompt after compaction: %+v, %v", req, err)
	}
}

func TestTrimToolOutputPreservesUTF8(t *testing.T) {
	text := strings.Repeat("пример 🐈\n", 1000)
	got := trimToolOutput(text)
	if !utf8.ValidString(got) || len(got) > 2048 || !strings.HasPrefix(got, "пример") || !strings.HasSuffix(got, "🐈\n") {
		t.Fatalf("invalid trimmed text: %q", got)
	}
}
