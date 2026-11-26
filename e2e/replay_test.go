package e2e

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		len(live) < 2 || live[len(live)-1].State != "running" || !live[len(live)-2].is("session_info_update") ||
		live[len(live)-2].Title != "answer" || live[len(live)-2].UpdatedAt == "" {
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
	check("session_info_update", "state_update idle") // also catches a marker leaked from session/new
	params["replayFrom"] = nil
	check("session_info_update", "state_update idle")
	for _, bad := range []any{"start", map[string]any{}, map[string]any{"type": "_unknown"}} {
		params["replayFrom"] = bad
		a.fails("session/resume", params, -32602)
	}
	params["replayFrom"] = map[string]any{"type": "start"}
	for range 2 {
		check("replay_complete", "session_info_update", "state_update idle")
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
			if got := kinds(a.untilIdle(sid)); !slices.Equal(got, []string{"replay_complete", "session_info_update", "state_update idle"}) {
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

func TestReplayOverflowPreservesLiveStream(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	run := make(chan struct{})
	var want strings.Builder
	for i := range 70 {
		fmt.Fprintf(&want, "[%02d]", i)
	}
	llm.enqueue(func(s *stream) {
		s.text("start")
		select {
		case <-run:
		case <-s.r.Context().Done():
			return
		}
		for i := range 70 {
			s.text(fmt.Sprintf("[%02d]", i))
		}
		s.finish("stop")
	})
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	old := mcpConfig("previous", filepath.Join(cwd, "calls"), "before replay")
	pidFile := filepath.Join(cwd, "pid")
	old["env"] = append(old["env"].([]any), map[string]string{"name": "FIXTURE_PID", "value": pidFile})
	sid := mcpSession(t, a, cwd, old)
	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	a.accepted(a.prompt(sid, "stream"))
	for !a.nextUpdate(sid).is("agent_message_chunk") {
	}
	resume, _ := gatedReplay(t, a, sid, cwd)
	params := map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}}
	a.fails("session/resume", params, 1001)
	close(run)
	rejected := a.response(resume)
	if rejected.Error == nil || rejected.Error.Code != -32603 {
		t.Fatalf("overflow response: %+v", rejected)
	}
	var got strings.Builder
	updates := a.untilIdle(sid)
	for _, u := range updates {
		if u.is("replay_complete") {
			t.Fatal("incomplete replay sent marker")
		}
		if u.is("agent_message_chunk") {
			if u.seq <= rejected.seq {
				t.Fatal("live chunk preceded overflow error")
			}
			got.WriteString(u.text())
		}
	}
	if got.String() != want.String() {
		t.Fatalf("overflow lost or reordered live chunks: %q", got.String())
	}
	if info := filter(updates, "session_info_update"); len(info) != 1 || info[0].Title != "" || info[0].UpdatedAt == "" || info[0].seq <= rejected.seq {
		t.Fatalf("live metadata after replay overflow: %+v", info)
	}
	a.ok("session/resume", params, nil)
	if got, err := os.ReadFile(pidFile); err != nil || string(got) != string(pid) {
		t.Fatalf("overflow retired the previous MCP process: pid %q -> %q, %v", pid, got, err)
	}
	retry := a.untilIdle(sid)
	answers := filter(retry, "agent_message")
	if len(filter(retry, "replay_complete")) != 1 || len(answers) != 1 || answers[0].text() != "start"+want.String() {
		t.Fatalf("retry history: %v", kinds(retry))
	}
}

func TestRetakeOrdersCancelledIdleAfterReplayMarker(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("write_file", `{"path":"file","content":"pending"}`))
	dir := t.TempDir()
	a, sid, cwd := started(t, llm, withDataDir(dir), askingUser)
	a.accepted(a.prompt(sid, "retiring turn"))
	permission := a.permissionRequest()
	db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Simulate an expired, different owner. Commit after the reader has
	// registered replay so the old turn's final idle belongs behind its marker.
	if _, err := tx.Exec("UPDATE sessions SET owner = 'other', lease_until = 0 WHERE id = ?", sid); err != nil {
		t.Fatal(err)
	}
	resume := a.request("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}})
	a.fails("_test/barrier", nil, -32601)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var updates []update
	for {
		m := a.take(func(m *message) bool {
			return (m.seq > permission.seq && m.Method == "session/update" && sessionOf(m) == sid) || (m.Method == "" && string(m.ID) == resume)
		})
		if m.Method == "" {
			if m.Error != nil {
				t.Fatal(m.Error)
			}
			break
		}
		updates = append(updates, decodeUpdate(t, m))
	}
	got := kinds(updates)
	want := []string{"user_message", "tool_call_update failed", "replay_complete", "tool_call_update cancelled", "state_update idle cancelled", "session_info_update", "state_update idle"}
	if !slices.Equal(got, want) {
		t.Fatalf("updates before resume response = %v", got)
	}
	if cancel := a.cancellation(permission); cancel.seq >= updates[len(updates)-3].seq {
		t.Fatal("permission cancellation followed the retiring turn's idle")
	}
	a.ok("session/close", map[string]any{"sessionId": sid}, nil)
}
