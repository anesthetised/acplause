package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/tools"
)

func TestTerminalStreamsAndResumesBeforeExit(t *testing.T) {
	model := make(scriptedModel, 2)
	model.callTool("shell", testShellName(), testShellArgs(`printf '\342'; while [ ! -f release ]; do sleep 0.01; done; printf '\202\254\000\377' >&2`, `$out = [Console]::OpenStandardOutput(); $out.WriteByte(226); $out.Flush(); while (!(Test-Path release)) { Start-Sleep -Milliseconds 10 }; $err = [Console]::OpenStandardError(); $err.Write([byte[]](130,172,0,255), 0, 4); $err.Flush()`))
	model.reply("done", llm.FinishStop, nil)
	s, out := testSession(t, model)
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("run")})
	var live acp.TerminalOutputChunk
	deadline := time.After(5 * time.Second)
	for live.TerminalID == "" {
		select {
		case u := <-out:
			switch v := u.(type) {
			case acp.TerminalOutputChunk:
				live = v
			case acp.StateUpdate:
				if v.State == acp.StateIdle {
					t.Fatal("exited before release")
				}
			}
		case <-deadline:
			t.Fatal("no live output")
		}
	}
	if b, err := base64.StdEncoding.DecodeString(live.Data); err != nil || !bytes.Equal(b, []byte{0xe2}) {
		t.Fatalf("live bytes %x, %v", b, err)
	}
	if err := s.Resume(context.Background(), nil, nil, true); err != nil {
		t.Fatal(err)
	}
	var replay acp.TerminalUpdate
	for replay.TerminalID == "" {
		select {
		case u := <-out:
			if v, ok := u.(acp.TerminalUpdate); ok {
				replay = v
			}
		case <-deadline:
			t.Fatal("no replay")
		}
	}
	if replay.TerminalID != live.TerminalID || replay.Output == nil || replay.Output.Data != live.Data || replay.ExitStatus != nil {
		t.Fatalf("replay: %+v", replay)
	}
	if err := os.WriteFile(filepath.Join(s.cwd, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	updates := out.untilIdle(t)
	ended := false
	for _, u := range updates {
		switch v := u.(type) {
		case acp.TerminalOutputChunk:
			if ended {
				t.Fatal("chunk after terminal finalization")
			}
		case acp.TerminalUpdate:
			if v.ExitStatus != nil {
				ended = true
				data, err := base64.StdEncoding.DecodeString(v.Output.Data)
				if err != nil || !bytes.Equal(data, []byte{0xe2, 0x82, 0xac, 0, 0xff}) || v.ExitStatus.ExitCode == nil || *v.ExitStatus.ExitCode != 0 {
					t.Fatalf("final: %+v, %x, %v", v, data, err)
				}
			}
		case acp.ToolCallUpdate:
			if v.Status == acp.ToolCompleted {
				if !ended || len(v.Content) != 2 || v.Content[0].Type != "terminal" || v.Content[0].TerminalID != live.TerminalID {
					t.Fatalf("tool completed before terminal: %+v", v)
				}
			}
		}
	}
	if !ended {
		t.Fatal("no exit snapshot")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	terminals := 0
	for _, u := range s.transcript {
		if _, ok := u.(acp.TerminalOutputChunk); ok {
			t.Fatal("stored intermediate chunk")
		}
		if v, ok := u.(acp.TerminalUpdate); ok {
			terminals++
			if v.ExitStatus == nil {
				t.Fatal("unfinished snapshot")
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("stored %d terminal rows", terminals)
	}
	for _, m := range s.history {
		if m.Role == llm.RoleTool && m.ToolCallID == "shell" && bytes.Contains([]byte(m.Content), []byte("4oKsAP8=")) {
			t.Fatal("base64 in model history")
		}
	}
}

func TestTerminalPersistenceAndRecovery(t *testing.T) {
	for _, exited := range []bool{false, true} {
		u := acp.TerminalUpdate{TerminalID: "t", Command: "printf test", Cwd: t.TempDir(), Output: &acp.TerminalOutput{Data: "AP8="}}
		if exited {
			u.ExitStatus = &acp.TerminalExitStatus{ExitCode: new(uint32(0))}
		}
		row, err := encodeEntry(u)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := decodeEntry(row)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(restored)
		b, _ := json.Marshal(u)
		if !bytes.Equal(a, b) {
			t.Fatalf("round trip: %s, want %s", a, b)
		}
		s, _ := testSession(t, nil)
		s.transcript = []acp.Update{acp.ToolCallUpdate{ToolCallID: "c", Status: acp.ToolInProgress, Content: []acp.ToolCallContent{acp.ToolTerminal("t")}}, restored}
		s.history = append(s.history, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "model-call"}}})
		s.repair()
		terminal := s.transcript[1].(acp.TerminalUpdate)
		if terminal.ExitStatus == nil || terminal.Output.Data != u.Output.Data || (terminal.ExitStatus.ExitCode == nil) == exited {
			t.Fatalf("recovery: %+v", terminal)
		}
		tool := s.transcript[0].(acp.ToolCallUpdate)
		if tool.Status != acp.ToolFailed || len(tool.Content) != 2 || tool.Content[0].TerminalID != "t" || s.history[len(s.history)-1].ToolCallID != "model-call" {
			t.Fatalf("repaired tool: %+v", tool)
		}
	}
}

func TestTerminalMethodsDoNotMixSessions(t *testing.T) {
	model := fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == llm.RoleUser {
			args, _ := json.Marshal(map[string]string{"command": testShellCommand("printf "+last.Content+"; while [ ! -f release ]; do sleep 0.01; done; printf end", "[Console]::Write('"+last.Content+"'); while (!(Test-Path release)) { Start-Sleep -Milliseconds 10 }; [Console]::Write('end')")})
			return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "shell", Function: llm.FunctionCall{Name: testShellName(), Arguments: string(args)}}, {ID: "sibling", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}}}}, nil
		}
		return &llm.Completion{Content: "done", FinishReason: llm.FinishStop}, nil
	})
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	dirs := []string{t.TempDir(), t.TempDir()}
	ids := []string{c.newSession(dirs[0]), c.newSession(dirs[1])}
	for i, id := range ids {
		c.selectModel(id, "m")
		if r := c.await(c.prompt(id, []string{"first", "second"}[i])); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	chunks := map[string]acp.TerminalOutputChunk{}
	deadline := time.After(10 * time.Second)
	for len(chunks) < 2 {
		select {
		case n := <-c.updates:
			if n.Update.Kind == "terminal_output_chunk" {
				var u acp.TerminalOutputChunk
				if err := json.Unmarshal(n.Update.Raw, &u); err != nil {
					t.Fatal(err)
				}
				chunks[n.SessionID] = u
			}
		case <-deadline:
			t.Fatal("concurrent sessions did not stream")
		}
	}
	for i, id := range ids {
		if got := string(decodeTerminalBytes(t, chunks[id].Data)); got != []string{"first", "second"}[i] {
			t.Fatalf("session %s received %q", id, got)
		}
	}
	if chunks[ids[0]].TerminalID == chunks[ids[1]].TerminalID {
		t.Fatal("shared terminal ID")
	}
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": ids[0], "cwd": dirs[0], "replayFrom": map[string]any{"type": "start"}}, nil)
	snapshot := c.waitFor(ids[0], func(n notification) bool { return n.Update.Kind == "terminal_update" })
	var replay acp.TerminalUpdate
	if err := json.Unmarshal(snapshot.Update.Raw, &replay); err != nil || replay.Output == nil || replay.Output.Data != chunks[ids[0]].Data {
		t.Fatalf("resume snapshot: %s, %v", snapshot.Update.Raw, err)
	}
	if err := os.WriteFile(filepath.Join(dirs[0], "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": ids[1]})
	idle := map[string]bool{}
	ended := map[string]bool{}
	for len(idle) < 2 {
		select {
		case n := <-c.updates:
			switch n.Update.Kind {
			case "terminal_update":
				var u acp.TerminalUpdate
				if err := json.Unmarshal(n.Update.Raw, &u); err != nil {
					t.Fatal(err)
				}
				if u.ExitStatus != nil {
					ended[n.SessionID] = true
					want := "firstend"
					if n.SessionID == ids[1] {
						want = "second"
					}
					if u.Output == nil || string(decodeTerminalBytes(t, u.Output.Data)) != want {
						t.Fatalf("mixed final output: %s", n.Update.Raw)
					}
				}
			case "terminal_output_chunk":
				if ended[n.SessionID] {
					t.Fatal("chunk after exit")
				}
			case "tool_call_update":
				if n.Update.Status == "completed" || n.Update.Status == "cancelled" {
					if !ended[n.SessionID] {
						t.Fatal("tool finalized before terminal")
					}
				}
			case "state_update":
				if n.Update.State == "idle" {
					if idle[n.SessionID] || !ended[n.SessionID] {
						t.Fatal("duplicate or premature idle")
					}
					idle[n.SessionID] = true
				}
			}
		case <-deadline:
			t.Fatal("sessions did not finish")
		}
	}
	c.agent.mu.Lock()
	second := c.agent.sessions[ids[1]]
	c.agent.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()
	results := map[string]int{}
	for _, m := range second.history {
		if m.Role == llm.RoleTool {
			results[m.ToolCallID]++
		}
	}
	if results["shell"] != 1 || results["sibling"] != 1 {
		t.Fatalf("cancelled sibling results: %v", results)
	}
}

func decodeTerminalBytes(t *testing.T, data string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// terminalPublicationClient injects backpressure and errors only at the
// publication boundary; the invocation's writer must remain independent.
type terminalPublicationClient struct {
	Client
	publish func(acp.Update) error
}

func (c terminalPublicationClient) Notify(method string, params any) error {
	if err := c.publish(params.(acp.SessionNotification).Update); err != nil {
		return err
	}
	return c.Client.Notify(method, params)
}

func TestTerminalSlowAndFailingPublication(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow", true: "failed"}[fail], func(t *testing.T) {
			s, out := testSession(t, nil)
			s.deps.log = slog.New(slog.DiscardHandler)
			blocked, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			s.deps.out = terminalPublicationClient{Client: out, publish: func(u acp.Update) error {
				if _, ok := u.(acp.TerminalOutputChunk); ok {
					once.Do(func() { close(blocked); <-release })
					if fail {
						return errors.New("client unavailable")
					}
				}
				return nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			command := testShellCommand(`printf ready; while [ ! -f produce ]; do sleep 0.01; done; printf '%1000000s' ''; touch drained; while [ ! -f release ]; do sleep 0.01; done`, `[Console]::Write('ready'); while (!(Test-Path produce)) { Start-Sleep -Milliseconds 10 }; [Console]::Write((' ' * 1000000)); [IO.File]::WriteAllText('drained', ''); while (!(Test-Path release)) { Start-Sleep -Milliseconds 10 }`)
			args, _ := json.Marshal(map[string]string{"command": command})
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.runTool(ctx, llm.ToolCall{ID: "shell", Function: llm.FunctionCall{Name: testShellName(), Arguments: string(args)}}, modelRound{})
			}()
			select {
			case <-blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("no publication")
			}
			if err := os.WriteFile(filepath.Join(s.cwd, "produce"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(s.cwd, "drained")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("publication stopped subprocess draining")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Cancellation reaches exec independently of the blocked publisher. Joining
			// publication then guarantees no chunks can escape after tool completion.
			cancel()
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("publication leaked its pump")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			var terminal acp.TerminalUpdate
			for _, u := range s.transcript {
				if v, ok := u.(acp.TerminalUpdate); ok {
					terminal = v
				}
			}
			data := decodeTerminalBytes(t, terminal.Output.Data)
			if terminal.ExitStatus == nil || !bytes.HasPrefix(data, []byte("ready")) || len(data) > 50100 || !bytes.Contains(data, []byte("950005 more bytes")) {
				t.Fatalf("bounded final snapshot: exit=%+v bytes=%d tail=%q", terminal.ExitStatus, len(data), data[max(0, len(data)-60):])
			}
		})
	}
}

func TestTerminalMethodsFailedStartsHaveNoTerminal(t *testing.T) {
	for _, denied := range []bool{true, false} {
		t.Run(map[bool]string{true: "denied", false: "start failed"}[denied], func(t *testing.T) {
			model := make(scriptedModel, 2)
			model.callTool("shell", testShellName(), testShellArgs(`touch ran`, `[IO.File]::WriteAllText('ran', '')`))
			model.reply("done", llm.FinishStop, nil)
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			cwd := t.TempDir()
			sid := c.newSession(cwd)
			c.selectModel(sid, "m")
			c.answer = func(context.Context) (string, string) {
				if denied {
					return acp.OutcomeSelected, optionReject
				}
				if err := os.Remove(cwd); err != nil {
					t.Error(err)
				}
				return acp.OutcomeSelected, optionAllow
			}
			if r := c.await(c.prompt(sid, "run")); r.Error != nil {
				t.Fatal(r.Error)
			}
			updates := c.untilIdle(sid)
			if hasKind(updates, "terminal_update") || hasKind(updates, "terminal_output_chunk") {
				t.Fatal("failed start created a terminal")
			}
		})
	}
}

func TestTerminalReplayPrecedesCompetingBytesAndExit(t *testing.T) {
	s, out := testSession(t, nil)
	insertPlanPrompt(t, s)
	entry := s.reportTool(-1, acp.ToolCallUpdate{ToolCallID: "tool", Status: acp.ToolPending})
	p := terminalPump{s: s, toolEntry: entry, toolID: "tool", entry: -1}
	p.start(tools.ShellSnapshot{Started: true, Command: "command", Cwd: s.cwd})
	p.publish(tools.ShellSnapshot{Output: []byte("before")})
	for len(out) > 0 {
		<-out
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	s.deps.out = terminalPublicationClient{Client: out, publish: func(u acp.Update) error {
		if _, ok := u.(acp.TerminalUpdate); ok {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}}
	resumed := make(chan error, 1)
	go func() { resumed <- s.Resume(context.Background(), nil, nil, true) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no replay")
	}
	updated := make(chan struct{})
	go func() {
		defer close(updated)
		p.publish(tools.ShellSnapshot{Output: []byte("after")})
		p.finish(tools.ShellSnapshot{Output: []byte("beforeafter")})
	}()
	unblock()
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume deadlocked")
	}
	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		t.Fatal("publication deadlocked")
	}
	if _, ok := (<-out).(acp.UserMessage); !ok {
		t.Fatal("missing user replay")
	}
	if _, ok := (<-out).(acp.ToolCallUpdate); !ok {
		t.Fatal("missing tool replay")
	}
	replay := (<-out).(acp.TerminalUpdate)
	if string(decodeTerminalBytes(t, replay.Output.Data)) != "before" || replay.ExitStatus != nil {
		t.Fatalf("stale replay: %+v", replay)
	}
	if _, ok := (<-out).(acp.StateUpdate); !ok {
		t.Fatal("bytes overtook replay state")
	}
	chunk := (<-out).(acp.TerminalOutputChunk)
	final := (<-out).(acp.TerminalUpdate)
	if string(decodeTerminalBytes(t, chunk.Data)) != "after" || string(decodeTerminalBytes(t, final.Output.Data)) != "beforeafter" || final.ExitStatus == nil || final.ExitStatus.ExitCode != nil {
		t.Fatalf("duplicated or lost bytes: %+v, %+v", chunk, final)
	}
	if len(s.transcript) != 3 || len(s.dirty) != 3 {
		t.Fatalf("stream created transcript rows: %d", len(s.transcript))
	}
}
