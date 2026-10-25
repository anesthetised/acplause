package e2e

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCommandDiscoveryOnNewAndResume(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t))
	a.initialize()
	cwd := t.TempDir()
	var created struct {
		SessionID string           `json:"sessionId"`
		Commands  []map[string]any `json:"availableCommands"`
	}
	a.ok("session/new", map[string]any{"cwd": cwd}, &created)
	if len(created.Commands) != 1 || created.Commands[0]["name"] != "compact" || created.Commands[0]["description"] == "" || created.Commands[0]["input"] != nil {
		t.Fatalf("commands: %+v", created.Commands)
	}
	var resumed struct {
		Commands []map[string]any `json:"availableCommands"`
	}
	a.ok("session/resume", map[string]any{"sessionId": created.SessionID, "cwd": cwd}, &resumed)
	if !reflect.DeepEqual(created.Commands, resumed.Commands) {
		t.Fatalf("resume commands: %+v", resumed.Commands)
	}
	updates := a.untilIdle(created.SessionID)
	if len(filter(updates, "available_commands_update")) != 0 {
		t.Fatal("static commands emitted a dynamic update")
	}
}

// seedCommandHistory stays below the automatic threshold, with a complete
// tool exchange that manual compaction must preserve in the summary input.
func seedCommandHistory(t *testing.T, llm *mockLLM, opts ...option) (*agent, string, string) {
	t.Helper()
	llm.enqueue(callTool("list_dir", `{}`), reply(strings.Repeat("old answer ", 30)))
	a, sid, cwd := started(t, llm, opts...)
	a.accepted(a.prompt(sid, "old request"))
	if updates := a.untilIdle(sid); updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("seed: %v", kinds(updates))
	}
	return a, sid, cwd
}

func TestRepeatedCompactCommandIsANoOp(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, _ := seedCommandHistory(t, llm)
	llm.enqueue(reply("Remember the inspected directory."))
	a.accepted(a.prompt(sid, "/compact"))
	if updates := a.untilIdle(sid); len(filter(updates, "compaction_update")) != 2 {
		t.Fatalf("first command: %v", kinds(updates))
	}
	requests := llm.requestCount()
	a.accepted(a.prompt(sid, "/compact"))
	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "end_turn" || len(filter(updates, "compaction_update")) != 0 || llm.requestCount() != requests {
		t.Fatalf("second command summarized again: %v", kinds(updates))
	}
	if answer := joinChunks(updates, "agent_message_chunk"); !strings.Contains(answer.text, "Nothing to compact") {
		t.Fatalf("no-op message: %+v", answer)
	}
}

func TestCompactCommandForcesAndPersistsFullReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	llm := newMockLLM(t, compactionModels)
	a, sid, cwd := seedCommandHistory(t, llm, withDataDir(dir), withEnv("ACPLAUSE_MAX_ROUNDS=2"))
	llm.enqueue(reply("Remember the inspected directory."))
	command := "  /compact\n"
	id, response := a.accepted(a.prompt(sid, command))
	updates := a.untilIdle(sid)
	if updates[0].MessageID != id || updates[0].text() != command || updates[0].seq < response.seq {
		t.Fatalf("command insertion: %v", kinds(updates))
	}
	compactions := filter(updates, "compaction_update")
	if len(compactions) != 2 || compactions[0].Status != "in_progress" || compactions[1].Status != "completed" || compactions[0].CompactionID != compactions[1].CompactionID {
		t.Fatalf("command compaction: %+v", compactions)
	}
	if answer := joinChunks(updates, "agent_message_chunk"); !strings.Contains(answer.text, "compacted") || strings.Contains(answer.text, "inspected") {
		t.Fatalf("confirmation: %+v", answer)
	}
	if idle := updates[len(updates)-1]; idle.StopReason != "end_turn" || len(filter(updates, "tool_call_update")) != 0 || llm.requestCount() != 3 {
		t.Fatalf("command ran tool loop: %v", kinds(updates))
	}
	if usage := filter(updates, "usage_update"); len(usage) != 1 || usage[0].Cost.Amount != 0.003 {
		t.Fatalf("summary cost: %+v", usage)
	}
	summary := llm.request(2)
	assertBoundedChat(t, summary)
	if len(summary.Tools) != 0 || !strings.Contains(summary.Messages[1]["content"].(string), "call_list_dir") || strings.Contains(summary.Messages[1]["content"].(string), "/compact") {
		t.Fatalf("summary input: %+v", summary)
	}
	a.stop()
	b := start(t, llm, withDataDir(dir))
	b.initialize()
	b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := b.untilIdle(sid)
	want := []string{"user_message", "tool_call_update completed", "agent_message", "user_message", "compaction_update", "agent_message", "state_update idle"}
	if !slices.Equal(kinds(replay), want) || replay[0].text() != "old request" || replay[2].text() != strings.Repeat("old answer ", 30) || replay[3].text() != command || replay[4].CompactionID != compactions[1].CompactionID || replay[4].Status != "completed" {
		t.Fatalf("full replay changed: %v", kinds(replay))
	}
	llm.enqueue(reply("next answer"))
	b.accepted(b.prompt(sid, "next"))
	if done := b.untilIdle(sid); done[len(done)-1].StopReason != "end_turn" {
		t.Fatalf("restart: %v", kinds(done))
	}
	data, _ := json.Marshal(llm.request(3).Messages)
	if strings.Contains(string(data), "old answer") || !strings.Contains(string(data), "Remember the inspected directory") || !strings.Contains(string(data), "/compact") {
		t.Fatal("restart resurrected old model context or lost command")
	}
}

func TestCompactCommandNoOp(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "no history", true: "unknown window"}[unknown], func(t *testing.T) {
			t.Parallel()
			models := compactionModels
			if unknown {
				models = `{"data":[{"id":"test/model"}]}`
			}
			llm := newMockLLM(t, models)
			a, sid, _ := started(t, llm)
			a.accepted(a.prompt(sid, "/compact"))
			updates := a.untilIdle(sid)
			text := joinChunks(updates, "agent_message_chunk").text
			if unknown && !strings.Contains(text, "no known context window") || !unknown && !strings.Contains(text, "no older conversation") || llm.requestCount() != 0 || len(filter(updates, "compaction_update")) != 0 || updates[len(updates)-1].StopReason != "end_turn" {
				t.Fatalf("no-op: %q; %v", text, kinds(updates))
			}
		})
	}
}

func TestCommandLikePromptsStayOrdinary(t *testing.T) {
	for _, kind := range []string{"compactness", "args", "unknown", "resource", "resource link", "two texts", "image"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, imageModelsJSON)
			llm.enqueue(reply("ordinary answer"))
			a, sid, _ := started(t, llm)
			text := map[string]any{"type": "text", "text": "/compact"}
			blocks := []any{text}
			switch kind {
			case "compactness":
				text["text"] = "/compactness"
			case "args":
				text["text"] = "/compact explanation"
			case "unknown":
				text["text"] = "/unknown"
			case "resource":
				blocks = []any{map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///command", "text": "/compact"}}}
			case "resource link":
				blocks = []any{map[string]any{"type": "resource_link", "uri": "file:///command", "name": "/compact"}}
			case "two texts":
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			case "image":
				blocks = append(blocks, promptImage(t, "image/png"))
			}
			a.accepted(a.request("session/prompt", imageParams(sid, blocks)))
			updates := a.untilIdle(sid)
			if joinChunks(updates, "agent_message_chunk").text != "ordinary answer" || len(filter(updates, "compaction_update")) != 0 || llm.requestCount() != 1 {
				t.Fatalf("intercepted ordinary prompt: %v", kinds(updates))
			}
			if len(llm.request(0).Tools) == 0 {
				t.Fatal("ordinary prompt became a summary")
			}
		})
	}
}

func TestCompactCommandCancelsBeforeInsertion(t *testing.T) {
	for _, sessionCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "request cancellation", true: "session cancellation"}[sessionCancel], func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, compactionModels)
			release, aborted := make(chan struct{}), make(chan struct{})
			llm.enqueue(waitFor(release, aborted))
			a, sid, cwd := started(t, llm)
			a.accepted(a.prompt(sid, "first"))
			a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
			command := a.prompt(sid, "/compact")
			if sessionCancel {
				a.notify("session/cancel", map[string]any{"sessionId": sid})
			} else {
				a.notify("$/cancel_request", map[string]any{"requestId": requestID(t, command)})
			}
			if err := a.response(command).Error; err == nil || err.Code != -32800 {
				t.Fatalf("queued command: %+v", err)
			}
			if !sessionCancel {
				// Session cancellation must abort the stream; releasing it
				// would race normal completion against that assertion.
				close(release)
			}
			updates := a.untilIdle(sid)
			want := "end_turn"
			if sessionCancel {
				want = "cancelled"
				waitClosed(t, aborted, "active request did not abort")
			}
			if updates[len(updates)-1].StopReason != want || len(filter(updates, "compaction_update")) != 0 || llm.requestCount() != 1 {
				t.Fatalf("pre-insertion cancellation: %v", kinds(updates))
			}
			a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := a.untilIdle(sid)
			if users := filter(replay, "user_message"); len(users) != 1 || users[0].text() != "first" {
				t.Fatalf("rejected command in replay: %v", kinds(replay))
			}
		})
	}
}

func TestCompactCommandQueuesBehindAcceptedTurn(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	release := make(chan struct{})
	llm.enqueue(waitFor(release, nil), reply("Earlier task summary."))
	a, sid, _ := started(t, llm, withEnv("ACPLAUSE_MAX_ROUNDS=1"))
	a.accepted(a.prompt(sid, "first"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	command := a.prompt(sid, "/compact")
	a.expectQuiet(func(m *message) bool { return string(m.ID) == command })
	close(release)
	first := a.untilIdle(sid)
	_, response := a.accepted(command)
	updates := a.untilIdle(sid)
	if response.seq < first[len(first)-1].seq || updates[len(updates)-1].StopReason != "end_turn" || llm.requestCount() != 2 || len(filter(updates, "compaction_update")) != 2 {
		t.Fatalf("queued command: %v", kinds(updates))
	}
	if req := llm.request(1); len(req.Tools) != 0 || !strings.Contains(req.Messages[1]["content"].(string), "working") {
		t.Fatal("queued compaction missed the preceding answer")
	}
}

func TestCompactCommandCancelsAfterInsertionAndPreservesHistory(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, cwd := seedCommandHistory(t, llm)
	aborted := make(chan struct{})
	llm.enqueue(waitFor(nil, aborted))
	a.accepted(a.prompt(sid, "/compact"))
	start := a.updateWhere(sid, func(u update) bool { return u.is("compaction_update") })
	llm.request(2)
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	updates := a.untilIdle(sid)
	end := filter(updates, "compaction_update")
	if len(end) != 1 || end[0].Status != "cancelled" || end[0].CompactionID != start.CompactionID || updates[len(updates)-1].StopReason != "cancelled" || len(filter(updates, "agent_message_chunk")) != 0 {
		t.Fatalf("cancelled command: %v", kinds(updates))
	}
	waitClosed(t, aborted, "summary HTTP stream was not aborted")
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := a.untilIdle(sid)
	if got := filter(replay, "compaction_update"); len(got) != 1 || got[0].Status != "cancelled" {
		t.Fatalf("cancelled replay: %v", kinds(replay))
	}
	llm.enqueue(reply("Recovered summary."))
	a.accepted(a.prompt(sid, "/compact"))
	if done := a.untilIdle(sid); done[len(done)-1].StopReason != "end_turn" {
		t.Fatalf("retry: %v", kinds(done))
	}
	if text := llm.request(3).Messages[1]["content"].(string); !strings.Contains(text, "old answer") || !strings.Contains(text, "old request") {
		t.Fatal("cancelled command discarded history")
	}
}

func TestCompactCommandSummaryFailureWithFiniteBudget(t *testing.T) {
	for _, kind := range []string{"empty", "tool", "length"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, compactionModels)
			a, sid, _ := seedCommandHistory(t, llm, withEnv("ACPLAUSE_MAX_ROUNDS=2"))
			llm.enqueue(func(s *stream) {
				switch kind {
				case "empty":
					s.text(" ")
					s.finish("stop")
				case "tool":
					s.toolCall(0, "forbidden", testShellName(), testShellArgs(`touch should-not-exist`, `[IO.File]::WriteAllText('should-not-exist', '')`))
					s.finish("tool_calls")
				case "length":
					s.text("unfinished")
					s.finish("length")
				}
			})
			a.accepted(a.prompt(sid, "/compact"))
			updates := a.untilIdle(sid)
			compactions := filter(updates, "compaction_update")
			idle := updates[len(updates)-1]
			if len(compactions) != 2 || compactions[1].Status != "failed" || compactions[1].CompactionError == "" || idle.StopReason != "error" || idle.Error == nil || llm.requestCount() != 3 || len(filter(updates, "tool_call_update")) != 0 || len(filter(updates, "agent_message_chunk")) != 0 {
				t.Fatalf("failed command: %v", kinds(updates))
			}
			llm.enqueue(reply("done"))
			a.accepted(a.prompt(sid, "next"))
			if done := a.untilIdle(sid); done[len(done)-1].StopReason != "end_turn" {
				t.Fatal("failure damaged next turn")
			}
			wire, _ := json.Marshal(llm.request(3).Messages)
			if !strings.Contains(string(wire), "old answer") || !strings.Contains(string(wire), "/compact") {
				t.Fatal("failed command replaced history")
			}
		})
	}
}
