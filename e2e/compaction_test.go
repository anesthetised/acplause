package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const compactionModels = `{"data":[{"id":"test/model","context_length":32000,"top_provider":{"max_completion_tokens":4096},"supported_parameters":["tools"]}]}`

// seedCompaction leaves enough history that the next prompt needs a summary.
func seedCompaction(t *testing.T, llm *mockLLM, opts ...option) (*agent, string, string) {
	t.Helper()
	llm.enqueue(callTool("list_dir", `{}`), reply(strings.Repeat("old answer ", 1200)))
	a, sid, cwd := started(t, llm, opts...)
	a.accepted(a.prompt(sid, strings.Repeat("old request ", 650)))
	if updates := a.untilIdle(sid); updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("seed turn: %v", kinds(updates))
	}
	return a, sid, cwd
}

func assertBoundedChat(t *testing.T, req chatRequest) {
	t.Helper()
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 4096 || uint64(len(wire))+req.MaxTokens > 32000 {
		t.Fatalf("request does not fit: %d input bytes, %d output tokens", len(wire), req.MaxTokens)
	}
	pending := map[string]bool{}
	for _, m := range req.Messages {
		if m["role"] == "tool" {
			id, _ := m["tool_call_id"].(string)
			if !pending[id] {
				t.Fatalf("orphan result %q", id)
			}
			delete(pending, id)
			continue
		}
		if len(pending) > 0 {
			t.Fatalf("unanswered calls: %v", pending)
		}
		calls, _ := m["tool_calls"].([]any)
		for _, c := range calls {
			pending[c.(map[string]any)["id"].(string)] = true
		}
	}
	if len(pending) > 0 {
		t.Fatalf("unanswered calls at request end: %v", pending)
	}
}

func TestCompactionReportsFitsAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t, compactionModels)
	a, sid, cwd := seedCompaction(t, llm, withDataDir(dir), withEnv("ACPLAUSE_MAX_ROUNDS=0"))
	llm.enqueue(reply("Keep the plan; the directory was inspected."), callTool("list_dir", `{}`), reply("done"))
	a.accepted(a.prompt(sid, "continue"))
	updates := a.untilIdle(sid)
	compactions := filter(updates, "compaction_update")
	if len(compactions) != 2 || compactions[0].Status != "in_progress" || compactions[1].Status != "completed" ||
		compactions[0].CompactionID == "" || compactions[0].CompactionID != compactions[1].CompactionID ||
		!strings.Contains(string(compactions[1].Summary), "Keep the plan") || updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("compaction lifecycle: %+v; turn: %v", compactions, kinds(updates))
	}
	if answer := joinChunks(updates, "agent_message_chunk"); answer.text != "done" {
		t.Fatalf("summary leaked into the answer: %+v", answer)
	}
	if usage := filter(updates, "usage_update"); len(usage) != 3 || usage[2].Cost.Amount != 0.005 {
		t.Fatalf("summary cost missing: %+v", usage)
	}
	for i := range 5 {
		assertBoundedChat(t, llm.request(i))
	}
	if summary := llm.request(2); len(summary.Tools) != 0 || !strings.Contains(summary.Messages[1]["content"].(string), "call_list_dir") {
		t.Fatalf("summary did not receive tool history: %+v", summary)
	}
	if next := llm.request(3); !strings.Contains(next.Messages[1]["content"].(string), "Keep the plan") || next.Messages[2]["content"] != "continue" {
		t.Fatalf("compacted model history: %+v", next.Messages)
	}
	a.stop()

	b := start(t, llm, withDataDir(dir))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := b.untilIdle(sid)
	got := filter(replay, "compaction_update")
	if len(got) != 1 || got[0].CompactionID != compactions[1].CompactionID || got[0].Status != "completed" || string(got[0].Summary) != string(compactions[1].Summary) {
		t.Fatalf("compaction replay: %+v", got)
	}
	want := []string{"user_message", "tool_call_update completed", "agent_message", "user_message", "compaction_update", "tool_call_update completed", "agent_message", "state_update idle"}
	if !slices.Equal(kinds(replay), want) || len(replay[0].text()) < 7000 || len(replay[2].text()) < 12000 {
		t.Fatalf("original timeline was changed: %v", kinds(replay))
	}
	llm.enqueue(reply("after restart"))
	b.accepted(b.prompt(sid, "next"))
	if done := b.untilIdle(sid); done[len(done)-1].StopReason != "end_turn" {
		t.Fatalf("turn after restart: %v", kinds(done))
	}
	req := llm.request(5)
	assertBoundedChat(t, req)
	wire, _ := json.Marshal(req.Messages)
	if strings.Contains(string(wire), "old answer") || !strings.Contains(string(wire), "Keep the plan") {
		t.Fatal("restart resurrected the pre-compaction history")
	}
}

func TestCompactionCanBeResumedAndCancelled(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, cwd := seedCompaction(t, llm)
	aborted := make(chan struct{})
	llm.enqueue(waitFor(make(chan struct{}), aborted))
	a.accepted(a.prompt(sid, "continue"))
	start := a.updateWhere(sid, func(u update) bool { return u.is("compaction_update") })
	llm.request(2)
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replayed := a.updateWhere(sid, func(u update) bool { return u.is("compaction_update") })
	if replayed.CompactionID != start.CompactionID || replayed.Status != "in_progress" {
		t.Fatalf("active compaction replay: %+v", replayed)
	}
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	updates := a.untilIdle(sid)
	end := filter(updates, "compaction_update")
	if len(end) != 1 || end[0].Status != "cancelled" || end[0].CompactionID != start.CompactionID || updates[len(updates)-1].StopReason != "cancelled" {
		t.Fatalf("cancelled compaction: %+v; %v", end, kinds(updates))
	}
	select {
	case <-aborted:
	case <-time.After(timeout):
		t.Fatal("summary HTTP stream was not cancelled")
	}
	llm.enqueue(reply("Recovered summary."), reply("done"))
	a.accepted(a.prompt(sid, "try again"))
	if done := a.untilIdle(sid); done[len(done)-1].StopReason != "end_turn" {
		t.Fatalf("recovery after cancellation: %v", kinds(done))
	}
	if data := llm.request(3).Messages[1]["content"].(string); !strings.Contains(data, "old answer") || !strings.Contains(data, "old request") {
		t.Fatal("cancelled compaction discarded the old history")
	}
}

func TestCompactionFailureEndsTurnWithError(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, _ := seedCompaction(t, llm)
	llm.enqueue(reply(" "))
	a.accepted(a.prompt(sid, "continue"))
	updates := a.untilIdle(sid)
	compactions := filter(updates, "compaction_update")
	if len(compactions) != 2 || compactions[1].Status != "failed" || compactions[1].CompactionError == "" {
		t.Fatalf("compaction failure missing: %+v", compactions)
	}
	idle := updates[len(updates)-1]
	if idle.StopReason != "error" || idle.Error == nil || llm.requestCount() != 3 {
		t.Fatalf("failed summary sent another request: %+v", idle)
	}
}

func TestCompactionTrimsToolOutputBeforeSummary(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, cwd := started(t, llm)
	large := strings.Repeat("file content ", 8000)
	if err := os.WriteFile(filepath.Join(cwd, "large.txt"), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	llm.enqueue(callTool("read_file", `{"path":"large.txt"}`), reply("done"))
	a.accepted(a.prompt(sid, "read the file"))
	updates := a.untilIdle(sid)
	if len(filter(updates, "compaction_update")) != 0 || updates[len(updates)-1].StopReason != "end_turn" || llm.requestCount() != 2 {
		t.Fatalf("trimming should suffice: %v", kinds(updates))
	}
	req := llm.request(1)
	assertBoundedChat(t, req)
	result := req.Messages[len(req.Messages)-1]["content"].(string)
	if len(result) > 2048 || !strings.Contains(result, "omitted") {
		t.Fatalf("untrimmed tool result: %d bytes", len(result))
	}
	calls := filter(updates, "tool_call_update")
	if calls[len(calls)-1].text() != large {
		t.Fatal("trimming changed the editor's tool output")
	}
}

func TestCompactionRejectsOversizedPromptBeforeHTTP(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, _ := started(t, llm)
	a.accepted(a.prompt(sid, strings.Repeat("x", 32000)))
	updates := a.untilIdle(sid)
	idle := updates[len(updates)-1]
	if idle.StopReason != "error" || idle.Error == nil || llm.requestCount() != 0 {
		t.Fatalf("oversized prompt reached provider: %+v", idle)
	}
}

func TestCompactionCountsAgainstTurnLimit(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, _ := seedCompaction(t, llm, withEnv("ACPLAUSE_MAX_ROUNDS=2"))
	llm.enqueue(reply("Retained summary."), callTool("list_dir", `{}`))
	a.accepted(a.prompt(sid, "continue"))
	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "max_turn_requests" || idle.Error != nil || llm.requestCount() != 4 {
		t.Fatalf("summary did not consume a request: %+v, requests=%d", idle, llm.requestCount())
	}
	compactions := filter(updates, "compaction_update")
	if len(compactions) != 2 || compactions[1].Status != "completed" {
		t.Fatalf("unexpected compaction lifecycle: %+v", compactions)
	}
	llm.enqueue(reply("done"))
	a.accepted(a.prompt(sid, "finish"))
	if done := a.untilIdle(sid); done[len(done)-1].StopReason != "end_turn" {
		t.Fatalf("turn after the limit: %v", kinds(done))
	}
	assertBoundedChat(t, llm.request(4))
}
