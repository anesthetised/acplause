package e2e

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestPromptLifecycle(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(func(s *stream) {
		s.keepalive()
		s.reasoning("Let me ")
		s.reasoning("think.")
		s.keepalive()
		s.text("Hello")
		s.text(", world")
		s.finish("stop")
	})
	a, sid, cwd := started(t, llm)

	id := a.request("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt": []any{
			map[string]any{"type": "text", "text": "Say hi", "annotations": map[string]any{"priority": 1}},
			map[string]any{"type": "resource_link", "uri": "file://" + cwd + "/main.go", "name": "main.go"},
			map[string]any{"type": "_vendor_block", "payload": 7},
		},
		"_meta": map[string]any{"traceparent": "00-1-2-01"},
	})
	messageID, resp := a.accepted(id)
	updates := a.untilIdle(sid)

	want := []string{"user_message", "session_info_update", "state_update running", "agent_thought_chunk", "agent_message_chunk", "usage_update", "state_update idle end_turn"}
	if got := kinds(updates); !slices.Equal(got, want) {
		t.Fatalf("updates:\n got %v\nwant %v", got, want)
	}
	user := updates[0]
	if user.MessageID != messageID || resp.seq > user.seq {
		t.Fatalf("user_message %q (line %d) must follow the prompt response %q (line %d)", user.MessageID, user.seq, messageID, resp.seq)
	}
	// The prompt is echoed verbatim, unknown blocks and fields included.
	var content []map[string]any
	_ = json.Unmarshal(user.Content, &content)
	if len(content) != 3 || content[2]["type"] != "_vendor_block" || content[0]["annotations"] == nil {
		t.Fatalf("user_message content: %s", user.Content)
	}

	thought, answer := joinChunks(updates, "agent_thought_chunk"), joinChunks(updates, "agent_message_chunk")
	if thought.text != "Let me think." || answer.text != "Hello, world" || thought.id == answer.id {
		t.Fatalf("thought %+v, answer %+v", thought, answer)
	}
	if u := find(t, updates, "usage_update"); u.Used != 15 || u.Size != 64000 || u.Cost == nil || u.Cost.Amount != 0.001 || u.Cost.Currency != "USD" {
		t.Fatalf("usage_update: %+v", u)
	}

	req := llm.request(0)
	if req.Model != "test/model" || !req.Stream || len(req.Tools) != 7 || req.Reasoning != nil {
		t.Fatalf("chat request: %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0]["role"] != "system" || req.Messages[1]["content"] != "Say hi\n\n[main.go](file://"+cwd+"/main.go)" {
		t.Fatalf("chat messages: %v", req.Messages)
	}
}

// find returns the first update of a kind.
func find(t *testing.T, updates []update, kind string) update {
	t.Helper()
	for _, u := range updates {
		if u.is(kind) {
			return u
		}
	}
	t.Fatalf("no %s in %v", kind, kinds(updates))
	return update{}
}

type chunks struct{ id, text string }

func joinChunks(updates []update, kind string) chunks {
	var c chunks
	for _, u := range updates {
		if u.is(kind) {
			if c.id != "" && c.id != u.MessageID {
				return chunks{id: "mixed ids"}
			}
			c.id = u.MessageID
			c.text += u.text()
		}
	}
	return c
}

func TestEachRoundIsANewAgentMessage(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(
		func(s *stream) {
			s.text("Looking.")
			s.toolCall(0, "call_1", "list_dir", `{}`)
			s.finish("tool_calls")
		},
		reply("Done."),
	)
	a, sid, _ := started(t, llm)
	a.accepted(a.prompt(sid, "go"))
	var ids []string
	for _, u := range a.untilIdle(sid) {
		if u.is("agent_message_chunk") {
			ids = append(ids, u.MessageID)
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("agent message ids per round: %v", ids)
	}
}

func TestStopReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		script script
		stop   string
		code   int
		kind   string
	}{
		{"length", func(s *stream) { s.text("trunc"); s.finish("length") }, "max_tokens", 0, ""},
		{"content filter", func(s *stream) { s.finish("content_filter") }, "refusal", 0, ""},
		{"mid-stream error", func(s *stream) {
			s.text("partial")
			s.data(map[string]any{
				"error":   map[string]any{"code": "server_error", "message": "Provider disconnected unexpectedly"},
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ""}, "finish_reason": "error"}},
			})
		}, "error", -32603, "provider"},
		{"truncated stream", func(s *stream) { s.text("partial") }, "error", -32603, "truncated"},
		{"unknown model", func(s *stream) {
			s.w.WriteHeader(404)
			s.raw(`{"error":{"code":404,"message":"No endpoints found"}}`)
		}, "error", -32603, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(tt.script)
			a, sid, _ := started(t, llm)
			a.accepted(a.prompt(sid, "hi"))
			updates := a.untilIdle(sid)
			idle := updates[len(updates)-1]
			if idle.StopReason != tt.stop {
				t.Fatalf("stop reason %q, want %q", idle.StopReason, tt.stop)
			}
			if tt.code == 0 {
				if idle.Error != nil {
					t.Fatalf("unexpected error: %+v", idle.Error)
				}
				return
			}
			var data struct {
				Kind string `json:"kind"`
			}
			if idle.Error == nil || idle.Error.Code != tt.code || json.Unmarshal(idle.Error.Data, &data) != nil || data.Kind != tt.kind {
				t.Fatalf("error %+v, want code %d kind %s", idle.Error, tt.code, tt.kind)
			}
		})
	}
}

func TestAuthFailureAsksForAuthentication(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(func(s *stream) {
		s.w.WriteHeader(401)
		s.raw(`{"error":{"code":401,"message":"expired key"}}`)
	})
	a, sid, _ := started(t, llm)
	a.accepted(a.prompt(sid, "hi"))
	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "error" || idle.Error.Code != -32000 {
		t.Fatalf("idle: %+v", idle)
	}
}

func TestModelCatalogAuthFailureAsksForAuthentication(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t), withEnv("ACPLAUSE_API_KEY=wrong"))
	a.initialize()
	a.fails("session/new", map[string]any{"cwd": t.TempDir()}, -32000)
}

func TestCancelMidStream(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	aborted := make(chan struct{})
	llm.enqueue(waitFor(nil, aborted), reply("still here"))
	a, sid, cwd := started(t, llm)

	a.accepted(a.prompt(sid, "long task"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	a.notify("session/cancel", map[string]any{"sessionId": sid})

	updates := a.untilIdle(sid)
	if idle := updates[len(updates)-1]; idle.StopReason != "cancelled" || idle.Error != nil {
		t.Fatalf("idle: %+v", idle)
	}
	waitClosed(t, aborted, "the model request was not aborted")
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	replay := a.untilIdle(sid)
	answers := filter(replay, "agent_message")
	if len(answers) != 1 || answers[0].text() != "working" || replay[len(replay)-1].StopReason != "cancelled" {
		t.Fatalf("cancelled partial response was lost: %v", replay)
	}

	// The session keeps working after a cancellation.
	a.accepted(a.prompt(sid, "again"))
	if updates := a.untilIdle(sid); updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("follow-up turn: %v", kinds(updates))
	}
}

func TestQueuedPromptsRunInOrder(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	release := make(chan struct{})
	llm.enqueue(waitFor(release, nil), reply("second"))
	a, sid, _ := started(t, llm)

	first := a.prompt(sid, "first")
	a.accepted(first)
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	second := a.prompt(sid, "second")
	a.expectQuiet(func(m *message) bool { return string(m.ID) == second })

	close(release)
	firstTurn := a.untilIdle(sid)
	_, resp := a.accepted(second)
	secondTurn := a.untilIdle(sid)

	idle := firstTurn[len(firstTurn)-1]
	if idle.StopReason != "end_turn" || resp.seq < idle.seq {
		t.Fatalf("second prompt accepted at line %d, before the first turn ended at line %d", resp.seq, idle.seq)
	}
	want := []string{"user_message", "state_update running", "agent_message_chunk", "usage_update", "state_update idle end_turn"}
	if got := kinds(secondTurn); !slices.Equal(got, want) {
		t.Fatalf("second turn: %v", got)
	}
	if msgs := llm.request(1).Messages; len(msgs) != 4 || msgs[2]["content"] != "working" || msgs[3]["content"] != "second" {
		t.Fatalf("second request history: %v", msgs)
	}
}

func TestCancelRequestWithdrawsAQueuedPrompt(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	release := make(chan struct{})
	llm.enqueue(waitFor(release, nil))
	a, sid, _ := started(t, llm)

	a.accepted(a.prompt(sid, "first"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	second := a.prompt(sid, "second")
	a.notify("$/cancel_request", map[string]any{"requestId": requestID(t, second)})
	if e := a.response(second).Error; e == nil || e.Code != -32800 {
		t.Fatalf("withdrawn prompt: %+v", e)
	}

	close(release)
	if updates := a.untilIdle(sid); updates[len(updates)-1].StopReason != "end_turn" {
		t.Fatalf("first turn: %v", kinds(updates))
	}
	a.expectQuiet(func(m *message) bool { return m.Method == "session/update" })
	if n := llm.requestCount(); n != 1 {
		t.Fatalf("withdrawn prompt reached the model: %d requests", n)
	}
}

func TestSessionCancelDropsQueuedPrompts(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	aborted := make(chan struct{})
	llm.enqueue(waitFor(nil, aborted))
	a, sid, _ := started(t, llm)

	a.accepted(a.prompt(sid, "first"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	second := a.prompt(sid, "second")
	a.notify("session/cancel", map[string]any{"sessionId": sid})

	if e := a.response(second).Error; e == nil || e.Code != -32800 {
		t.Fatalf("queued prompt: %+v", e)
	}
	if updates := a.untilIdle(sid); updates[len(updates)-1].StopReason != "cancelled" {
		t.Fatalf("first turn: %v", kinds(updates))
	}
	waitClosed(t, aborted, "the model request was not aborted")
}

func requestID(t *testing.T, id string) int {
	t.Helper()
	n, err := strconv.Atoi(id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func waitClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatal(msg)
	}
}

func TestEmbeddedResourcesReachTheModel(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(reply("ok"))
	a, sid, _ := started(t, llm)

	blocks := []any{
		map[string]any{"type": "text", "text": "Review this"},
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///w/a.go", "mimeType": "text/x-go", "text": "package a\n"}},
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///w/x.png", "mimeType": "image/png", "blob": "AAEC"}},
	}
	a.accepted(a.request("session/prompt", map[string]any{"sessionId": sid, "prompt": blocks}))
	updates := a.untilIdle(sid)

	var echoed []map[string]any
	_ = json.Unmarshal(updates[0].Content, &echoed)
	if len(echoed) != 3 || echoed[1]["type"] != "resource" || echoed[2]["resource"].(map[string]any)["blob"] != "AAEC" {
		t.Fatalf("user_message content: %s", updates[0].Content)
	}
	want := "Review this\n\n<resource uri=\"file:///w/a.go\">\npackage a\n\n</resource>\n\n[binary resource omitted: file:///w/x.png]"
	if got := llm.request(0).Messages[1]["content"]; got != want {
		t.Fatalf("model message:\n got %q\nwant %q", got, want)
	}
}
