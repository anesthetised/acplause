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
		{"unicode limit", []acp.PlanEntry{{Content: strings.Repeat("\u00e9", 1000), Priority: "low", Status: "pending"}}, true},
		{"nil", nil, false},
		{"empty", []acp.PlanEntry{}, false},
		{"too many", slices.Repeat([]acp.PlanEntry{valid[1]}, 21), false},
		{"empty content", []acp.PlanEntry{{Priority: "low", Status: "pending"}}, false},
		{"blank content", []acp.PlanEntry{{Content: " \n\t", Priority: "low", Status: "pending"}}, false},
		{"too long", []acp.PlanEntry{{Content: strings.Repeat("\u00e9", 1001), Priority: "low", Status: "pending"}}, false},
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
	expectReplayCompleteAndInfo(t, out)
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

func TestStoredPlanRemovalReadsAsEmptySnapshotOnOlderAgents(t *testing.T) {
	want := acp.PlanRemoved{PlanID: "removed-id"}
	row, err := encodeEntry(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeEntry(row)
	if err != nil || got != want {
		t.Fatalf("stored removal = %#v, %v", got, err)
	}
	// Read through the old envelope and plan decoder, including its raw
	// round trip. An empty snapshot without the marker is ordinary history.
	var old struct {
		Kind   string          `json:"kind"`
		Update json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(row, &old); err != nil || old.Kind != "plan_update" {
		t.Fatalf("old envelope = %+v, %v", old, err)
	}
	legacyRow, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	u, err := decodeEntry(legacyRow)
	plan, ok := u.(acp.PlanUpdate)
	if err != nil || !ok || plan.Plan.Type != "items" || plan.Plan.PlanID != want.PlanID || plan.Plan.Entries == nil || len(plan.Plan.Entries) != 0 {
		t.Fatalf("empty legacy plan = %#v, %v", u, err)
	}
	if _, err := decodeEntry([]byte(`{"kind":"plan_update","update":{"plan":{"type":"items","entries":[]}},"planRemoved":true}`)); err == nil {
		t.Fatal("decoded a removal without an ID")
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
	expectReplayCompleteAndInfo(t, out)
	if _, ok := (<-out).(acp.StateUpdate); !ok {
		t.Fatal("live plan overtook resume state")
	}
	if u := (<-out).(acp.PlanUpdate); u.Plan.Entries[0].Content != "after" {
		t.Fatalf("live snapshot: %+v", u)
	}
}

func TestPlanRemovalRequiresExplicitCancellationOfUnfinishedWork(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		stop         acp.StopReason
		remove       bool
	}{
		{"cancel pending", "pending", acp.StopCancelled, true},
		{"cancel in progress", "in_progress", acp.StopCancelled, true},
		{"cancel completed", "completed", acp.StopCancelled, false},
		{"provider failure", "in_progress", acp.StopError, false},
		{"close", "in_progress", acp.StopCancelled, false},
		{"disconnect", "in_progress", acp.StopCancelled, false},
		{"ownership loss", "in_progress", acp.StopCancelled, false},
		{"deadline", "in_progress", acp.StopCancelled, false},
		{"cancel after deadline", "in_progress", acp.StopCancelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s *Session
			expired, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			model := fakeModel(func(ctx context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				if _, err := s.updatePlan(ctx, []acp.PlanEntry{{Content: "work", Priority: "high", Status: tc.status}}); err != nil {
					return nil, err
				}
				if tc.stop == acp.StopError {
					return nil, errors.New("provider failed")
				}
				<-ctx.Done()
				if tc.name == "cancel after deadline" {
					close(expired)
					<-release
				}
				return nil, ctx.Err()
			})
			var out recorder
			s, out = testSession(t, model)
			s.deps.log = slog.New(slog.DiscardHandler)
			if strings.Contains(tc.name, "deadline") {
				s.deps.turnTimeout = time.Second
			}
			s.Submit(context.Background(), []acp.ContentBlock{acp.Text("work")})
			plan := waitPlan(t, out)
			switch tc.name {
			case "cancel pending", "cancel in progress", "cancel completed":
				s.Cancel()
				s.Cancel()
			case "close":
				s.Close()
			case "disconnect":
				s.cancel()
			case "ownership loss":
				s.markLost()
			case "cancel after deadline":
				select {
				case <-expired:
				case <-time.After(5 * time.Second):
					t.Fatal("deadline did not expire")
				}
				s.Cancel()
				release <- struct{}{}
			}
			updates := out.untilIdle(t)
			idle := updates[len(updates)-1].(acp.StateUpdate)
			if idle.StopReason != tc.stop {
				t.Fatalf("idle: %+v", idle)
			}
			removed := 0
			for _, u := range updates {
				if r, ok := u.(acp.PlanRemoved); ok {
					removed++
					if r.PlanID != plan.Plan.PlanID {
						t.Fatalf("removed another plan: %+v", r)
					}
				}
			}
			s.mu.Lock()
			snapshot := s.transcript[s.planIndex[plan.Plan.PlanID]]
			s.mu.Unlock()
			if tc.remove {
				if removed != 1 || snapshot != (acp.PlanRemoved{PlanID: plan.Plan.PlanID}) {
					t.Fatalf("removals = %d, current transcript slot = %#v", removed, snapshot)
				}
			} else if removed != 0 || !reflect.DeepEqual(snapshot, plan) {
				t.Fatalf("removals = %d, preserved snapshot = %#v", removed, snapshot)
			}
		})
	}
}

func waitPlan(t *testing.T, out recorder) acp.PlanUpdate {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-out:
			if p, ok := u.(acp.PlanUpdate); ok {
				return p
			}
		case <-deadline:
			t.Fatal("turn did not publish a plan")
		}
	}
}

func TestPlanRemovalAcrossReplayCut(t *testing.T) {
	for _, tc := range []struct {
		name         string
		removeBefore bool
	}{
		{"before replay cut", true},
		{"after replay cut", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			model := make(scriptedModel, 4)
			request := make(chan llm.Request, 1)
			const args = `{"entries":[{"content":"work","priority":"low","status":"pending"}]}`
			model.callTool("prior", "update_plan", args)
			model.reply("last turn", llm.FinishStop, nil)
			model.callTool("current", "update_plan", args)
			model <- func(ctx context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				request <- req
				<-ctx.Done()
				return nil, ctx.Err()
			}
			s, out := testSession(t, model)
			s.Submit(ctx, []acp.ContentBlock{acp.Text("prior")})
			prior := waitPlan(t, out)
			out.untilIdle(t)
			s.Submit(ctx, []acp.ContentBlock{acp.Text("current")})
			current := waitPlan(t, out)
			if current.Plan.PlanID == prior.Plan.PlanID {
				t.Fatal("accepted turn reused the prior plan ID")
			}
			select {
			case req := <-request:
				assertToolPairs(t, req.Messages)
			case <-time.After(5 * time.Second):
				t.Fatal("model did not receive the current plan tool result")
			}
			r, err := s.deps.replays.begin(ctx, s.id, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer r.cancel()
			s.mu.Lock()
			worker := s.worker
			s.mu.Unlock()
			if !tc.removeBefore {
				r.snapshot(s)
			}
			s.Cancel()
			select {
			case <-worker:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled worker did not finish")
			}
			if tc.removeBefore {
				r.snapshot(s)
			}
			if err := r.write(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.finish(ctx, nil); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, u := range out.untilIdle(t) {
				switch v := u.(type) {
				case acp.PlanUpdate:
					got = append(got, "plan "+v.Plan.PlanID)
				case acp.PlanRemoved:
					got = append(got, "removed "+v.PlanID)
				case acp.ReplayComplete:
					got = append(got, "marker")
				case acp.StateUpdate:
					if v.State == acp.StateIdle {
						got = append(got, string(v.StopReason))
					}
				}
			}
			want := []string{"plan " + prior.Plan.PlanID}
			if tc.removeBefore {
				want = append(want, "removed "+current.Plan.PlanID, "marker")
			} else {
				want = append(want, "plan "+current.Plan.PlanID, "marker", "removed "+current.Plan.PlanID)
			}
			want = append(want, string(acp.StopCancelled))
			if !slices.Equal(got, want) || len(out) != 0 {
				t.Fatalf("replay order = %v, want %v (tail: %d)", got, want, len(out))
			}
		})
	}
}

func TestPlanRemovalWhileSavePending(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"commit", nil},
		{"ordinary failure", errors.New("refused plan removal")},
		{"ownership loss", store.ErrLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var s *Session
			model := fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				if _, err := s.updatePlan(ctx, []acp.PlanEntry{{Content: "work", Priority: "low", Status: "pending"}}); err != nil {
					return nil, err
				}
				<-ctx.Done()
				return nil, ctx.Err()
			})
			s, out := testSession(t, model)
			s.saves.Wait()
			s.deps.log = slog.New(slog.DiscardHandler)
			entered, release := make(chan store.Session, 1), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			attempts, idleSaves := 0, 0
			s.deps.store = saveStore{save: func(ctx context.Context, sess store.Session, _ []store.Row, transcript []store.Row) error {
				hasPlan := false
				for _, row := range transcript {
					got, err := decodeEntry(row.Data)
					if err != nil {
						t.Error(err)
					}
					switch got.(type) {
					case acp.PlanRemoved:
						attempts++
						if row.Seq != 1 || row.Key != "" || ctx.Err() != nil {
							t.Errorf("incorrect plan removal row or context: %+v, %v", row, ctx.Err())
						}
						entered <- sess
						<-release
						return tc.err
					case acp.PlanUpdate:
						hasPlan = true
					}
				}
				if sess.State == string(acp.StateIdle) {
					idleSaves++
					if hasPlan != (tc.err != nil) {
						t.Errorf("idle save wrote original plan = %t after removal save returned %v", hasPlan, tc.err)
					}
				}
				return nil
			}}
			s.Submit(ctx, []acp.ContentBlock{acp.Text("work")})
			plan := waitPlan(t, out)
			s.mu.Lock()
			worker := s.worker
			s.mu.Unlock()
			s.Cancel()
			select {
			case sess := <-entered:
				if sess.State != string(acp.StateRunning) {
					t.Fatalf("pending removal save described session state %q", sess.State)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("removal did not reach the store")
			}
			for len(out) != 0 {
				u := <-out
				if _, ok := u.(acp.SessionInfoUpdate); !ok {
					t.Fatalf("published %#v while removal save was blocked", u)
				}
			}

			// The running state and original snapshot remain visible, and a
			// repeated cancellation can be handled while SQLite is blocked.
			resumed := make(chan error, 1)
			go func() {
				s.Cancel()
				resumed <- s.Resume(ctx, nil, nil, true)
			}()
			select {
			case err := <-resumed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("session handlers blocked on plan removal persistence")
			}
			var replay []acp.Update
			for len(out) != 0 {
				u := <-out
				if _, ok := u.(acp.SessionInfoUpdate); !ok {
					replay = append(replay, u)
				}
			}
			if len(replay) != 4 || !reflect.DeepEqual(replay[1], plan) ||
				replay[2] != (acp.ReplayComplete{}) || replay[3] != (acp.StateUpdate{State: acp.StateRunning}) {
				t.Fatalf("replay while the removal save was pending: %#v", replay)
			}
			unblock()

			updates := out.untilIdle(t)
			if got := updates[len(updates)-1]; got != (acp.StateUpdate{State: acp.StateIdle, StopReason: acp.StopCancelled}) {
				t.Fatalf("final update = %#v", got)
			}
			select {
			case <-worker:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled worker did not finish")
			}
			var want acp.Update = plan
			wantIdleSaves := 1
			switch tc.err {
			case nil:
				want = acp.PlanRemoved{PlanID: plan.Plan.PlanID}
				if len(updates) != 2 || updates[0] != want {
					t.Fatalf("want one removal before idle: %#v", updates)
				}
			case store.ErrLost:
				wantIdleSaves = 0
				if len(updates) != 1 || !s.isLost() {
					t.Fatalf("lost session final updates: %#v", updates)
				}
			default:
				if len(updates) != 2 {
					t.Fatalf("want one save warning before idle: %#v", updates)
				}
				notice, ok := updates[0].(acp.Notice)
				if !ok || notice.Severity != "warning" {
					t.Fatalf("invalid save warning: %#v", updates[0])
				}
			}
			got := s.transcript[s.planIndex[plan.Plan.PlanID]]
			if !reflect.DeepEqual(got, want) || attempts != 1 || idleSaves != wantIdleSaves || len(out) != 0 {
				t.Fatalf("transcript slot = %#v, removal saves = %d, idle saves = %d, extra updates = %d",
					got, attempts, idleSaves, len(out))
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
	_, _, _, _, err = s.pending(nil)
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
	if len(out) != notifications+4 {
		t.Fatal("identical replacement did not publish plan and metadata updates")
	}
	got := s.transcript[1].(acp.PlanUpdate)
	if len(s.transcript) != 3 || got.Plan.PlanID != id || !reflect.DeepEqual(got.Plan.Entries, entries) || len(s.history) != 2 {
		t.Fatalf("replacement changed position/history: %+v", s.transcript)
	}
	_, _, rows, _, err := s.pending(nil)
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
