package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

func TestValidatePlan(t *testing.T) {
	valid := []acp.PlanEntry{{Content: "Implement", Priority: "high", Status: "in_progress"}, {Content: "Verify", Priority: "low", Status: "pending"}}
	for _, tt := range []struct {
		name    string
		entries []acp.PlanEntry
		ok      bool
	}{
		{"valid", valid, true},
		{"completed", []acp.PlanEntry{{Content: "done", Priority: "medium", Status: "completed"}}, true},
		{"twenty", slices.Repeat([]acp.PlanEntry{valid[1]}, 20), true},
		{"unicode limit", []acp.PlanEntry{{Content: strings.Repeat("я", 1000), Priority: "low", Status: "pending"}}, true},
		{"nil", nil, false},
		{"empty", []acp.PlanEntry{}, false},
		{"too many", slices.Repeat([]acp.PlanEntry{valid[1]}, 21), false},
		{"empty content", []acp.PlanEntry{{Priority: "low", Status: "pending"}}, false},
		{"blank content", []acp.PlanEntry{{Content: " \n\t", Priority: "low", Status: "pending"}}, false},
		{"too long", []acp.PlanEntry{{Content: strings.Repeat("я", 1001), Priority: "low", Status: "pending"}}, false},
		{"missing priority", []acp.PlanEntry{{Content: "x", Status: "pending"}}, false},
		{"future priority", []acp.PlanEntry{{Content: "x", Priority: "_urgent", Status: "pending"}}, false},
		{"missing status", []acp.PlanEntry{{Content: "x", Priority: "low"}}, false},
		{"future status", []acp.PlanEntry{{Content: "x", Priority: "low", Status: "_paused"}}, false},
		{"cancelled", []acp.PlanEntry{{Content: "x", Priority: "low", Status: "cancelled"}}, false},
		{"two active", slices.Repeat([]acp.PlanEntry{valid[0]}, 2), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validatePlan(tt.entries); (err == nil) != tt.ok {
				t.Fatalf("validatePlan = %v, want valid=%t", err, tt.ok)
			}
		})
	}
}

func TestPlanSnapshotsPersistAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cat := newCatalog(nil)
	s, err := newSession(ctx, deps{out: make(recorder, 32), store: st, log: slog.New(slog.DiscardHandler)}, cat, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := st.Create(ctx, s.record()); err != nil {
		t.Fatal(err)
	}
	entries := []acp.PlanEntry{{Content: "unfinished", Priority: "high", Status: "in_progress"}}
	insertPlanPrompt(t, s)
	first, err := s.updatePlan(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	s.flush()
	entries[0].Status = "pending"
	if _, err := s.updatePlan(ctx, entries); err != nil {
		t.Fatal(err)
	}
	s.flush()
	insertPlanPrompt(t, s)
	entries[0].Status = "in_progress"
	second, err := s.updatePlan(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	s.flush()
	if err := st.Release(ctx, s.id); err != nil {
		t.Fatal(err)
	}
	next, err := store.Open(dir, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	loaded, err := next.Acquire(ctx, s.id)
	if err != nil {
		t.Fatal(err)
	}
	out := make(recorder, 32)
	restored, err := restoreSession(ctx, deps{out: out, store: next}, cat, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.planID != "" || restored.planIndex[first] != 1 || restored.planIndex[second] != 3 || len(restored.planIndex) != 2 || len(restored.transcript) != 4 {
		t.Fatalf("restored indexes: current=%q, %+v, %+v", restored.planID, restored.planIndex, restored.transcript)
	}
	if err := restored.Resume(ctx, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	for i, want := range s.transcript {
		got := <-out
		before, _ := json.Marshal(want)
		after, _ := json.Marshal(got)
		if string(before) != string(after) {
			t.Fatalf("replayed entry %d changed: %s, want %s", i, after, before)
		}
	}
	if state := (<-out).(acp.StateUpdate); state.State != acp.StateIdle {
		t.Fatalf("restart state: %+v", state)
	}
	if len(restored.dirty) != 0 {
		t.Fatal("recovery rewrote unfinished plan statuses")
	}
	insertPlanPrompt(t, restored)
	third, err := restored.updatePlan(ctx, entries)
	if err != nil || third == first || third == second || third == "" {
		t.Fatalf("restart reused a historical plan: %q, %v", third, err)
	}
}

func TestStoredPlanPreservesFutureFields(t *testing.T) {
	for _, raw := range []string{
		`{"plan":{"type":"items","planId":"p","entries":[]},"future":true}`,
		`{"plan":{"type":"items","planId":"p","entries":[{"content":"","priority":"_future","status":"cancelled","future":true}]}}`,
		`{"plan":{"type":"_future","planId":"p","entries":false,"payload":{}}}`,
	} {
		row := []byte(`{"kind":"plan_update","update":` + raw + `}`)
		u, err := decodeEntry(row)
		if err != nil {
			t.Fatal(err)
		}
		back, err := encodeEntry(u)
		if err != nil || string(back) != string(row) {
			t.Fatalf("stored plan changed: %s, %v; want %s", back, err, row)
		}
	}
}

// replayPlanClient pauses replay inside Notify, while Session.mu is held.
type replayPlanClient struct {
	recorder
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *replayPlanClient) Notify(method string, params any) error {
	if _, ok := params.(acp.SessionNotification).Update.(acp.PlanUpdate); ok {
		c.once.Do(func() { close(c.entered); <-c.release })
	}
	return c.recorder.Notify(method, params)
}

func TestPlanResumePrecedesCompetingLiveUpdate(t *testing.T) {
	s, out := testSession(t, nil)
	insertPlanPrompt(t, s)
	entries := []acp.PlanEntry{{Content: "before", Priority: "low", Status: "pending"}}
	if _, err := s.updatePlan(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	for len(out) > 0 {
		<-out
	}
	client := &replayPlanClient{recorder: out, entered: make(chan struct{}), release: make(chan struct{})}
	s.deps.out = client
	var release sync.Once
	unblock := func() { release.Do(func() { close(client.release) }) }
	defer unblock()
	resumed := make(chan error, 1)
	go func() { resumed <- s.Resume(context.Background(), nil, nil, true) }()
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not reach plan replay")
	}
	updated := make(chan error, 1)
	go func() {
		_, err := s.updatePlan(context.Background(), []acp.PlanEntry{{Content: "after", Priority: "high", Status: "in_progress"}})
		updated <- err
	}()
	unblock()
	for _, done := range []chan error{resumed, updated} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("resume/live update deadlocked")
		}
	}
	if _, ok := (<-out).(acp.UserMessage); !ok {
		t.Fatal("replay lost the user message")
	}
	if u := (<-out).(acp.PlanUpdate); u.Plan.Entries[0].Content != "before" {
		t.Fatalf("replay snapshot: %+v", u)
	}
	if _, ok := (<-out).(acp.StateUpdate); !ok {
		t.Fatal("live plan overtook resume state")
	}
	if u := (<-out).(acp.PlanUpdate); u.Plan.Entries[0].Content != "after" {
		t.Fatalf("live snapshot: %+v", u)
	}
}

func TestPlanUnfinishedOnTurnFailureOrCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancelled"}[cancelled], func(t *testing.T) {
			var s *Session
			model := fakeModel(func(ctx context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				if _, err := s.updatePlan(ctx, []acp.PlanEntry{{Content: "unfinished", Priority: "high", Status: "in_progress"}}); err != nil {
					return nil, err
				}
				if cancelled {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, errors.New("provider failed")
			})
			var out recorder
			s, out = testSession(t, model)
			s.deps.log = slog.New(slog.DiscardHandler)
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("work")})
			if cancelled {
			waitPlan:
				for {
					select {
					case u := <-out:
						if _, ok := u.(acp.PlanUpdate); ok {
							s.Cancel()
							break waitPlan
						}
					case <-time.After(5 * time.Second):
						t.Fatal("turn did not publish a plan")
					}
				}
			}
			updates := out.untilIdle(t)
			idle := updates[len(updates)-1].(acp.StateUpdate)
			want := acp.StopError
			if cancelled {
				want = acp.StopCancelled
			}
			if idle.StopReason != want {
				t.Fatalf("idle: %+v", idle)
			}
			s.mu.Lock()
			plan := s.transcript[s.planIndex[s.planID]].(acp.PlanUpdate)
			s.mu.Unlock()
			if plan.Plan.Entries[0].Status != "in_progress" {
				t.Fatalf("turn ending completed an unfinished plan: %+v", plan)
			}
		})
	}
}

func TestPlanRejectsClosedOrLostSession(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "lost"}[lost], func(t *testing.T) {
			s, out := testSession(t, nil)
			insertPlanPrompt(t, s)
			count := len(out)
			if lost {
				s.markLost()
			} else {
				s.Close()
			}
			_, err := s.updatePlan(context.Background(), []acp.PlanEntry{{Content: "x", Priority: "low", Status: "pending"}})
			if err == nil || len(out) != count || len(s.transcript) != 1 || s.planID != "" {
				t.Fatalf("revoked session changed plan: %v, %+v", err, s.transcript)
			}
		})
	}
}

func insertPlanPrompt(t *testing.T, s *Session) {
	t.Helper()
	if !s.insert(context.Background(), &submission{
		ctx: context.Background(), prompt: []acp.ContentBlock{acp.Text("work")}, reply: jsonrpc.NewReply(context.Background()),
	}) {
		t.Fatal("prompt rejected")
	}
}

func TestPlanSnapshotsReplaceAtFirstPosition(t *testing.T) {
	s, out := testSession(t, nil)
	ctx := context.Background()
	entries := []acp.PlanEntry{{Content: "first", Priority: "high", Status: "in_progress"}}
	if _, err := s.updatePlan(ctx, entries); err == nil || len(out) != 0 || len(s.transcript) != 0 {
		t.Fatal("plan accepted before a user turn")
	}
	insertPlanPrompt(t, s)
	if _, err := s.updatePlan(ctx, nil); err == nil || s.planID != "" {
		t.Fatal("invalid first update allocated a plan")
	}
	id, err := s.updatePlan(ctx, entries)
	if err != nil || id == "" {
		t.Fatalf("first plan: %q, %v", id, err)
	}
	first := s.transcript[1].(acp.PlanUpdate)
	entries[0].Content = "caller changed"
	if first.Plan.Entries[0].Content != "first" {
		t.Fatal("plan retained the caller's mutable entries")
	}
	s.mu.Lock()
	s.transcript = append(s.transcript, acp.AgentMessage{MessageID: "a", Content: []acp.ContentBlock{acp.Text("progress")}})
	s.dirty[2] = true
	_, _, _, _, err = s.pending()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	entries = []acp.PlanEntry{{Content: "replacement", Priority: "low", Status: "pending"}, {Content: "done", Priority: "medium", Status: "completed"}}
	notifications := len(out)
	for range 2 { // An identical update is still a full replacement event.
		if got, err := s.updatePlan(ctx, entries); err != nil || got != id {
			t.Fatalf("replacement plan: %q, %v", got, err)
		}
	}
	if len(out) != notifications+2 {
		t.Fatal("identical replacement did not publish a new event")
	}
	got := s.transcript[1].(acp.PlanUpdate)
	if len(s.transcript) != 3 || got.Plan.PlanID != id || !reflect.DeepEqual(got.Plan.Entries, entries) || len(s.history) != 2 {
		t.Fatalf("replacement changed position/history: %+v", s.transcript)
	}
	_, _, rows, _, err := s.pending()
	if err != nil || len(rows) != 1 || rows[0].Seq != 1 {
		t.Fatalf("replacement was not dirty at original position: %+v, %v", rows, err)
	}
	before, _ := json.Marshal(s.transcript)
	count := len(out)
	if _, err := s.updatePlan(ctx, nil); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.updatePlan(cancelled, entries); err == nil {
		t.Fatal("cancelled update accepted")
	}
	if s.insert(cancelled, &submission{ctx: ctx, reply: jsonrpc.NewReply(context.Background())}) || s.planID != id {
		t.Fatal("rejected prompt reset the current plan")
	}
	after, _ := json.Marshal(s.transcript)
	if string(before) != string(after) || len(out) != count || len(s.dirty) != 0 {
		t.Fatal("rejected updates changed the snapshot")
	}
	insertPlanPrompt(t, s)
	next, err := s.updatePlan(ctx, entries)
	if err != nil || next == "" || next == id || len(s.transcript) != 5 || s.planIndex[id] != 1 || s.planIndex[next] != 4 {
		t.Fatalf("new turn reused historical plan: %q, %v, %+v", next, err, s.transcript)
	}
	if !reflect.DeepEqual(s.transcript[1], got) {
		t.Fatal("new turn changed historical plan")
	}
}
