package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

func TestCursorArmsBeforeAgentLookup(t *testing.T) {
	c := startAgent(t, t.TempDir(), nil, nil)
	c.initialize()
	cwd := t.TempDir()
	id := c.newSession(cwd)
	a := c.agent
	s, _ := a.session(id)
	a.mu.Lock()
	unlock := sync.OnceFunc(a.mu.Unlock)
	defer unlock()
	rs := a.deps.replays
	rs.mu.Lock()
	armed := rs.armed
	rs.mu.Unlock()
	resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "replayFrom": map[string]string{"type": "start"}})
	issue50Wait(t, armed)
	s.mu.Lock()
	s.send(acp.Notice{Title: "while lookup waits", Severity: "info"})
	s.mu.Unlock()
	unlock()
	if r := c.await(resume); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := c.untilIdle(id)
	if len(got) != 4 || got[0].Update.Kind != "replay_complete" || got[1].Update.Title != "while lookup waits" ||
		got[2].Update.Kind != "session_info_update" {
		t.Fatalf("receipt order: %+v", got)
	}
}

func replayTestSession(ctx context.Context, rs *replayStreams, id string) *Session {
	return &Session{id: id, ctx: ctx, deps: deps{replays: rs, out: rs.wire, log: slog.New(slog.DiscardHandler)}}
}

func beginTestReplay(t *testing.T, s *Session) *historyReplay {
	t.Helper()
	r, err := s.deps.replays.begin(t.Context(), s.id, jsonrpc.NewReply(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	r.snapshot(s)
	return r
}

func TestReplayLimitsChargeDetachedTails(t *testing.T) {
	// Reuse immutable encoded bytes across sessions to exercise the actual
	// 64-MiB aggregate without constructing 64 MiB of duplicate input.
	large := make([]byte, maxReplayBytes+1)
	for _, unit := range []string{"frames", "bytes", "oversized"} {
		t.Run(unit, func(t *testing.T) {
			release := make(chan struct{})
			entered := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			first := sync.OnceFunc(func() { close(entered); <-release })
			out := replayTestWriter(func(b []byte) (int, error) { first(); return len(b), nil })
			conn := jsonrpc.NewConn(nil, out, slog.New(slog.DiscardHandler))
			defer func() { unblock(); _ = conn.Close() }()
			rs := newReplayStreams(conn)
			if err := conn.Notify("hold", nil); err != nil {
				t.Fatal(err)
			}
			issue50Wait(t, entered)
			var replays []*historyReplay
			totalFrames, totalBytes := 0, 0
			for i := range 5 {
				s := replayTestSession(t.Context(), rs, fmt.Sprint(i))
				r := beginTestReplay(t, s)
				replays = append(replays, r)
				count, line := maxReplayFrames, []byte("{}")
				switch unit {
				case "bytes":
					count, line = 1, large[:maxReplayBytes-1]
				case "oversized":
					count, line = 1, large
				}
				for range count {
					_, err := rs.admitLive(liveUpdate{session: s, line: line}, nil)
					if err != nil {
						t.Fatal(err)
					}
					if r.err != nil {
						break
					}
					totalFrames++
					totalBytes += len(line) + 1
				}
				if (r.err == errReplayFull) != (unit == "oversized" || i == 4) {
					t.Fatalf("session %d: %v", i, r.err)
				}
			}
			if rs.frames != totalFrames || rs.bytes != totalBytes {
				t.Fatalf("held frames/bytes %d/%d, want %d/%d", rs.frames, rs.bytes, totalFrames, totalBytes)
			}
			for _, r := range replays {
				// The four unaffected sessions can complete with their markers.
				want := r.err
				if err := r.finish(r.ctx, nil); err != want {
					t.Fatalf("finish %s = %v, want %v", r.id, err, want)
				}
			}
			if rs.frames != totalFrames || rs.bytes != totalBytes || rs.reservations != 5 {
				t.Fatal("detached items released credits before dequeue")
			}
			unblock()
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if rs.frames != 0 || rs.bytes != 0 || rs.reservations != 0 {
				t.Fatal("writer leaked replay credits")
			}
		})
	}
}

func TestReplayCompletionReservationLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		out := replayTestWriter(func(b []byte) (int, error) { <-release; return len(b), nil })
		conn := jsonrpc.NewConn(nil, out, slog.New(slog.DiscardHandler))
		defer conn.Close()
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		if err := conn.Notify("hold", nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		rs := newReplayStreams(conn)
		for i := range maxReplayReservations {
			r := beginTestReplay(t, replayTestSession(t.Context(), rs, fmt.Sprint(i)))
			if err := r.finish(r.ctx, nil); err != nil {
				t.Fatal(err)
			}
		}
		if r, err := rs.begin(t.Context(), "full", nil); r != nil || err == nil || jsonrpc.AsError(err).Code == replayConflict {
			t.Fatalf("reservation limit = %+v, %v", r, err)
		}
		unblock()
		synctest.Wait()
		r := beginTestReplay(t, replayTestSession(t.Context(), rs, "retry"))
		if err := r.finish(r.ctx, nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOverflowFencesBlockedHistoryAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		out := &pausedOutput{release: release}
		conn := jsonrpc.NewConn(nil, out, slog.New(slog.DiscardHandler))
		defer conn.Close()
		drain := sync.OnceFunc(func() { close(release) })
		defer drain()
		rs := newReplayStreams(conn)
		s := replayTestSession(t.Context(), rs, "s")
		for range 300 {
			s.transcript = append(s.transcript, acp.AgentMessage{MessageID: "history"})
		}
		r := beginTestReplay(t, s)
		if err := r.stageCurrent(s); err != nil {
			t.Fatal(err)
		}
		history := make(chan error, 1)
		go func() { history <- r.write(r.ctx) }()
		synctest.Wait() // partial history fills ordinary capacity
		const updatedAt = "2026-11-27T00:00:00Z"
		s.send(acp.SessionInfoUpdate{UpdatedAt: updatedAt})
		const notices = maxReplayFrames - 2 // Two snapshots and one live patch already held.
		for i := range notices {
			s.send(acp.Notice{Severity: "info", Title: fmt.Sprint(i)})
		}
		if err := <-history; err == nil {
			t.Fatal("history escaped overflow cancellation")
		}
		if err := r.finish(r.ctx, nil); err != errReplayFull {
			t.Fatal(err)
		}
		later := make(chan struct{})
		go func() { s.send(acp.Notice{Severity: "info", Title: "later"}); close(later) }()
		drain()
		<-later
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		seenError, seenInfo := false, false
		var titles []string
		for line := range strings.Lines(out.String()) {
			var msg struct {
				Error  *jsonrpc.Error
				Params notification
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatal(err)
			}
			switch {
			case msg.Error != nil:
				if seenError {
					t.Fatal("duplicate resume error")
				}
				seenError = true
			case msg.Params.Update.Kind == "agent_message":
				if seenError {
					t.Fatal("stale history after overflow")
				}
			case msg.Params.Update.Kind == "notice":
				if !seenError {
					t.Fatal("live update before error")
				}
				titles = append(titles, msg.Params.Update.Title)
			case msg.Params.Update.Kind == "session_info_update":
				if !seenError || seenInfo || len(titles) != 0 || msg.Params.Update.UpdatedAt != updatedAt {
					t.Fatalf("aborted replay metadata out of order or synthetic: %s", line)
				}
				seenInfo = true
			default:
				t.Fatalf("unexpected frame: %s", line)
			}
		}
		var want []string
		for i := range notices {
			want = append(want, fmt.Sprint(i))
		}
		if !seenInfo || !slices.Equal(titles, append(want, "later")) {
			t.Fatalf("overflow order %v, live metadata delivered %t", titles, seenInfo)
		}
	})
}

type gatedMarshal struct{ block func() }

func (p gatedMarshal) MarshalJSON() ([]byte, error) { p.block(); return []byte("{}"), nil }

func TestPeerCallRechecksGateAtEnqueue(t *testing.T) {
	for _, method := range []string{acp.MethodSessionRequestPermission, acp.MethodElicitationCreate} {
		for _, withdraw := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel_before_%t", method, withdraw), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var output bytes.Buffer
					conn := jsonrpc.NewConn(nil, &output, slog.New(slog.DiscardHandler))
					defer conn.Close()
					rs := newReplayStreams(conn)
					s := replayTestSession(t.Context(), rs, "session")
					client := replayClient{conn, s}
					call := client.CallPermission
					if method == acp.MethodElicitationCreate {
						call = client.CallCancellable
					}
					started, release := make(chan struct{}), make(chan struct{})
					p := gatedMarshal{sync.OnceFunc(func() { close(started); <-release })}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					result := make(chan error, 1)
					go func() { result <- call(ctx, method, p, &struct{}{}) }()
					<-started // passed the optimistic check and claimed permission capacity
					r := beginTestReplay(t, s)
					close(release)
					synctest.Wait()
					if output.Len() != 0 {
						t.Fatalf("request crossed armed replay: %s", output.String())
					}
					if withdraw {
						cancel()
						synctest.Wait()
					}
					if err := r.finish(r.ctx, nil); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					cancel()
					if err := <-result; !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
					lines := strings.Split(strings.TrimSpace(output.String()), "\n")
					if !strings.Contains(lines[0], "replay_complete") {
						t.Fatalf("first line = %s", lines[0])
					}
					if withdraw {
						if len(lines) != 1 {
							t.Fatalf("unadmitted call reached peer: %q", lines)
						}
					} else {
						var req struct {
							ID     int
							Method string
						}
						var n struct{ Params struct{ RequestID int } }
						if len(lines) != 3 || json.Unmarshal([]byte(lines[1]), &req) != nil || json.Unmarshal([]byte(lines[2]), &n) != nil || req.Method != method || req.ID != n.Params.RequestID || req.ID == 0 {
							t.Fatalf("call or cancellation ID changed: %q", lines)
						}
					}
				})
			})
		}
	}
}

func TestLiveUpdateRechecksGateAtEnqueue(t *testing.T) {
	var output bytes.Buffer
	conn := jsonrpc.NewConn(nil, &output, slog.New(slog.DiscardHandler))
	defer conn.Close()
	rs := newReplayStreams(conn)
	s := replayTestSession(t.Context(), rs, "live")
	event, err := s.frame(acp.Notice{Severity: "info", Title: "held"})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := rs.admitLive(event, nil)
	if err != nil || changed == nil {
		t.Fatal("ordinary path did not reach capacity acquisition")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	sent := make(chan error, 1)
	go func() {
		sent <- conn.SendLine(t.Context(), event.line, changed, func(commit func() error) error {
			close(entered)
			<-release
			_, err := rs.admitLive(event, commit)
			return err
		})
	}()
	<-entered
	r := beginTestReplay(t, s)
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := r.finish(r.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "replay_complete") || !strings.Contains(lines[1], "held") {
		t.Fatalf("live order %q", lines)
	}
}

func TestDecliningReplayWithFullOutputKeepsReading(t *testing.T) {
	cwd := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		in, peer := io.Pipe()
		release := make(chan struct{})
		out := &pausedOutput{release: release}
		conn := jsonrpc.NewConn(in, out, slog.New(slog.DiscardHandler))
		drain := sync.OnceFunc(func() { close(release) })
		defer func() { drain(); _ = peer.Close(); _ = conn.Close() }()
		a := &Agent{ctx: t.Context(), deps: deps{replays: newReplayStreams(conn)}}
		seen := make(chan struct{})
		methods := jsonrpc.Methods{
			Requests:      map[string]jsonrpc.Handler{acp.MethodSessionResume: jsonrpc.Handle(a.resumeSession)},
			Notifications: map[string]jsonrpc.Handler{"seen": func(context.Context, json.RawMessage) (any, error) { close(seen); return nil, nil }},
		}
		go func() { _ = conn.Serve(t.Context(), methods) }()
		for range 257 {
			if err := conn.Notify("filled", nil); err != nil {
				t.Fatal(err)
			}
		}
		req := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  acp.MethodSessionResume,
			"params": map[string]any{
				"sessionId":  "s",
				"cwd":        cwd,
				"replayFrom": map[string]string{"type": "start"},
			},
		}
		if err := json.NewEncoder(peer).Encode(req); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(peer, `{"jsonrpc":"2.0","method":"seen"}`); err != nil {
			t.Fatal(err)
		}
		<-seen
		drain()
		a.jobs.Wait()
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if !strings.Contains(lines[len(lines)-1], "no replay completion slot") {
			t.Fatal("missing capacity rejection")
		}
	})
}
