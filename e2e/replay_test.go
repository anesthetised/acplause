package e2e

import "testing"

func TestResumeReplaysStreamingMessages(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	release := make(chan struct{})
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
		s.finish("stop")
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
	resp := a.ok("session/resume", params, nil)
	var replay []update
	for {
		u := a.nextUpdate(sid)
		replay = append(replay, u)
		if u.is("state_update") {
			if u.State != "running" || u.seq > resp.seq {
				t.Fatalf("resume state: %+v", u)
			}
			break
		}
	}
	thoughts, answers := filter(replay, "agent_thought"), filter(replay, "agent_message")
	if len(thoughts) != 1 || thoughts[0].MessageID != thoughtID || thoughts[0].text() != "Thinking " ||
		len(answers) != 1 || answers[0].MessageID != answerID || answers[0].text() != "Hello " {
		t.Fatalf("partial messages missing from replay: %v", replay)
	}
	close(release)
	live := a.untilIdle(sid)
	answer, thought := joinChunks(live, "agent_message_chunk"), joinChunks(live, "agent_thought_chunk")
	if answer.id != answerID || answer.text != "world." || thought.id != thoughtID || thought.text != "done." {
		t.Fatalf("continuation after replay: answer=%+v, thought=%+v", answer, thought)
	}
	a.ok("session/resume", params, nil)
	replay = a.untilIdle(sid)
	thoughts, answers = filter(replay, "agent_thought"), filter(replay, "agent_message")
	if len(thoughts) != 1 || thoughts[0].text() != "Thinking done." || len(answers) != 1 || answers[0].text() != "Hello world." {
		t.Fatalf("final replay duplicated or lost text: %v", replay)
	}
}
