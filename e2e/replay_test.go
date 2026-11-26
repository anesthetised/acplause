package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResumeReplaysStreamingMessages(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	release := make(chan struct{})
	finish := make(chan struct{})
	llm.enqueue(func(s *stream) {
		s.reasoning("Thinking ")
		s.text("Hello ")
		select {
		case <-release:
		case <-s.r.Context().Done():
			return
		}
		s.reasoning("done.")
		s.text("world.")
		s.toolCall(0, "inspect", "list_dir", `{"path":"."}`)
		s.finish("tool_calls")
	}, func(s *stream) {
		select {
		case <-finish:
			s.finish("stop")
		case <-s.r.Context().Done():
		}
	})
	a, sid, cwd := started(t, llm)
	a.accepted(a.prompt(sid, "answer"))
	var thoughtID, answerID string
	for answerID == "" {
		u := a.nextUpdate(sid)
		if u.is("agent_thought_chunk") {
			thoughtID = u.MessageID
		}
		if u.is("agent_message_chunk") {
			answerID = u.MessageID
		}
	}
	params := map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}
	resume, finishSetup := gatedReplay(t, a, sid, cwd)
	close(release)
	llm.request(1) // Both continuations and the read tool have passed through the worker.
	finishSetup()
	var replay, live []update
	markers := 0
	for {
		m := a.take(func(m *message) bool {
			return (m.Method == "session/update" && sessionOf(m) == sid) || (m.Method == "" && string(m.ID) == resume)
		})
		if m.Method == "" {
			if m.Error != nil {
				t.Fatal(m.Error)
			}
			break
		}
		u := decodeUpdate(t, m)
		switch {
		case u.is("replay_complete"):
			markers++
		case markers == 0:
			replay = append(replay, u)
		default:
			live = append(live, u)
		}
	}
	if markers != 1 || !slices.Equal(kinds(replay), []string{"user_message", "agent_thought", "agent_message"}) ||
		len(live) == 0 || live[len(live)-1].State != "running" {
		t.Fatalf("resume boundary: history %v, markers %d, live %v", kinds(replay), markers, kinds(live))
	}
	thoughts, answers := filter(replay, "agent_thought"), filter(replay, "agent_message")
	if len(thoughts) != 1 || thoughts[0].MessageID != thoughtID || thoughts[0].text() != "Thinking " ||
		len(answers) != 1 || answers[0].MessageID != answerID || answers[0].text() != "Hello " {
		t.Fatalf("partial messages missing from replay: %v", replay)
	}
	answer, thought := joinChunks(live, "agent_message_chunk"), joinChunks(live, "agent_thought_chunk")
	if answer.id != answerID || answer.text != "world." || thought.id != thoughtID || thought.text != "done." {
		t.Fatalf("continuation after replay: answer=%+v, thought=%+v", answer, thought)
	}
	close(finish)
	a.untilIdle(sid)
	a.ok("session/resume", params, nil)
	replay = a.untilIdle(sid)
	thoughts, answers = filter(replay, "agent_thought"), filter(replay, "agent_message")
	if len(thoughts) != 1 || thoughts[0].text() != "Thinking done." || len(answers) != 1 || answers[0].text() != "Hello world." {
		t.Fatalf("final replay duplicated or lost text: %v", replay)
	}
}

func TestReplayBoundaryForEmptyHistory(t *testing.T) {
	t.Parallel()
	a, sid, cwd := started(t, newMockLLM(t))
	params := map[string]any{"sessionId": sid, "cwd": cwd}
	check := func(want ...string) {
		t.Helper()
		resp := a.ok("session/resume", params, nil)
		updates := a.untilIdle(sid)
		if !slices.Equal(kinds(updates), want) || updates[len(updates)-1].seq >= resp.seq {
			t.Fatalf("resume updates = %v before response %d, want %v", kinds(updates), resp.seq, want)
		}
	}
	check("state_update idle") // also catches a marker leaked from session/new
	params["replayFrom"] = nil
	check("state_update idle")
	for _, bad := range []any{"start", map[string]any{}, map[string]any{"type": "_unknown"}} {
		params["replayFrom"] = bad
		a.fails("session/resume", params, -32602)
	}
	params["replayFrom"] = map[string]any{"type": "start"}
	for range 2 {
		check("replay_complete", "state_update idle")
	}
}

func TestColdPromptDuringReplaySetup(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[cancel], func(t *testing.T) {
			t.Parallel()
			a, sid, cwd := started(t, newMockLLM(t))
			a.ok("session/close", map[string]any{"sessionId": sid}, nil)
			request, finishSetup := gatedReplay(t, a, sid, cwd)
			prompt := map[string]any{"sessionId": sid, "prompt": []any{}, "idempotencyKey": "k"}
			a.sessionNotFound("session/prompt", prompt)
			if cancel {
				a.notify("$/cancel_request", map[string]any{"requestId": requestID(t, request)})
				if r := a.response(request); r.Error == nil || r.Error.Code != -32800 {
					t.Fatalf("cancelled cold resume: %+v", r)
				}
				a.sessionNotFound("session/prompt", prompt)
				// A clean retry also detects any leaked marker from cancellation.
				a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			} else {
				finishSetup()
				if r := a.response(request); r.Error != nil {
					t.Fatal(r.Error)
				}
			}
			if got := kinds(a.untilIdle(sid)); !slices.Equal(got, []string{"replay_complete", "state_update idle"}) {
				t.Fatalf("cold replay: %v", got)
			}
		})
	}
}

// gatedReplay returns once MCP setup has begun and keeps it pending until release.
func gatedReplay(t *testing.T, a *agent, sid, cwd string) (id string, release func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	cfg := mcpConfig("slow", gate+".calls", "replay")
	cfg["env"] = append(cfg["env"].([]any), map[string]string{"name": "FIXTURE_GATE", "value": gate})
	id = a.request("session/resume", map[string]any{
		"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}, "mcpServers": []any{cfg},
	})
	waitForFile(t, gate+".entered")
	return id, func() {
		t.Helper()
		if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplayOrdersNewClientRequests(t *testing.T) {
	for _, method := range []string{"session/request_permission", "elicitation/create"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			release := make(chan struct{})
			llm.enqueue(func(s *stream) {
				s.text("working")
				select {
				case <-release:
				case <-s.r.Context().Done():
					return
				}
				if method == "elicitation/create" {
					s.toolCall(0, "ask", "ask_user", formArgs)
				} else {
					s.toolCall(0, "write", "write_file", `{"path":"new.txt","content":"done"}`)
				}
				s.finish("tool_calls")
			}, reply("done"))
			a, sid, cwd := startedForms(t, llm, askingUser)
			a.accepted(a.prompt(sid, "go"))
			a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
			id, finishSetup := gatedReplay(t, a, sid, cwd)
			close(release)
			a.expectQuiet(func(m *message) bool { return m.Method == method })
			finishSetup()
			if resp := a.response(id); resp.Error != nil {
				t.Fatal(resp.Error)
			}
			marker := a.updateWhere(sid, func(u update) bool { return u.is("replay_complete") })
			req := a.take(func(m *message) bool { return m.Method == method })
			if req.seq <= marker.seq {
				t.Fatalf("%s preceded replay_complete", method)
			}
			if method == "elicitation/create" {
				a.answer(req, formAnswer("accept"))
			} else {
				a.answer(req, selected(optionOfKind(t, req, "allow_once")))
			}
			a.untilIdle(sid)
		})
	}
}
