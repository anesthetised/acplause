package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

// issue50GatedStore delays an ownership result or an old write at the actual
// Store boundary. Automatic renewal remains disabled unless explicitly armed.
type issue50GatedStore struct {
	Store
	saveGate, renewGate atomic.Bool
	saves               atomic.Int32
	saveEntered         chan struct{}
	renewEntered        chan struct{}
	acquireEntered      chan struct{}
	releaseSave         chan struct{}
	releaseRenew        chan struct{}
	saveResult          chan error
}

func (s *issue50GatedStore) Save(ctx context.Context, row store.Session, h, tr []store.Row) error {
	s.saves.Add(1)
	if !s.saveGate.CompareAndSwap(true, false) {
		return s.Store.Save(ctx, row, h, tr)
	}
	close(s.saveEntered)
	<-s.releaseSave
	err := s.Store.Save(ctx, row, h, tr)
	s.saveResult <- err
	return err
}

func (s *issue50GatedStore) Renew(ctx context.Context, ids []string) ([]string, error) {
	if !s.renewGate.CompareAndSwap(true, false) {
		return nil, nil
	}
	lost, err := s.Store.Renew(ctx, ids)
	close(s.renewEntered)
	<-s.releaseRenew
	return lost, err
}

func (s *issue50GatedStore) Acquire(ctx context.Context, id string) (*store.Loaded, error) {
	l, err := s.Store.Acquire(ctx, id)
	select {
	case s.acquireEntered <- struct{}{}:
	default:
	}
	return l, err
}

func issue50Wait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controlled operation did not finish")
	}
}

func issue50RaceOwners(t *testing.T) (*acpClient, *Session, *issue50GatedStore, string) {
	t.Helper()
	dir, cwd := t.TempDir(), t.TempDir()
	var gate *issue50GatedStore
	a := startAgent(t, dir, make(scriptedModel, 1), nil, func(st Store) Store {
		gate = &issue50GatedStore{Store: st, saveEntered: make(chan struct{}), renewEntered: make(chan struct{}), acquireEntered: make(chan struct{}, 1), releaseSave: make(chan struct{}), releaseRenew: make(chan struct{}), saveResult: make(chan error, 1)}
		return gate
	})
	a.initialize()
	id := a.newSession(cwd)
	a.selectModel(id, "m")
	old, _ := a.agent.session(id)
	issue50Flush(t, old)
	mb := make(scriptedModel, 1)
	b := startAgent(t, dir, mb, nil, func(st Store) Store { return issue50Store{st} })
	b.initialize()
	b.ok(acp.MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd}, nil)
	b.untilIdle(id)
	mb.reply("B retained answer", llm.FinishStop, nil)
	if response := b.await(b.prompt(id, "B retained prompt")); response.Error != nil {
		t.Fatal(response.Error)
	}
	b.untilIdle(id)
	current, _ := b.agent.session(id)
	issue50Flush(t, current)
	return a, old, gate, cwd
}

func TestIssue50RetakeDrainsOldSaves(t *testing.T) {
	for _, claim := range []bool{false, true} {
		name := "flush in flight"
		if claim {
			name = "claimTurn in flight"
		}
		t.Run(name, func(t *testing.T) { issue50RetakeDrainsOldSaves(t, claim) })
	}
}

func issue50RetakeDrainsOldSaves(t *testing.T, claim bool) {
	t.Helper()
	a, old, gate, cwd := issue50RaceOwners(t)
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate.releaseSave) }) }
	defer unblock()
	if !claim {
		old.mu.Lock()
		old.history = append(old.history, llm.Message{Role: llm.RoleUser, Content: "stale old write"})
		old.mu.Unlock()
	}
	gate.saveGate.Store(true)
	before := gate.saves.Load()
	flushed := make(chan struct{})
	var firstPrompt <-chan rpcResponse
	if claim {
		firstPrompt = a.prompt(old.id, "stale in-flight prompt")
	} else {
		go func() { old.flush(); close(flushed) }()
	}
	issue50Wait(t, gate.saveEntered)
	queued := make(chan struct{})
	go func() { old.flush(); close(queued) }()
	prompt := a.prompt(old.id, "stale queued prompt")
	// Fence the reader: the prompt worker has been admitted while the old
	// save still owns flushMu. Initialization must remain responsive.
	a.initialize()
	opened := make(chan *Session, 1)
	failed := make(chan error, 1)
	openDone := make(chan struct{})
	go func() {
		s, err := a.agent.open(context.Background(), old.id, cwd)
		opened <- s
		failed <- err
		close(openDone)
	}()
	select {
	case <-gate.acquireEntered:
		issue50Wait(t, openDone)
		t.Error("Acquire restored the process owner ID before the old save finished")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if claim {
		if response := a.await(firstPrompt); response.Error == nil {
			t.Error("old in-flight prompt crossed insertion after losing ownership")
		}
	} else {
		issue50Wait(t, flushed)
	}
	issue50Wait(t, queued)
	if err := <-gate.saveResult; !errors.Is(err, store.ErrLost) {
		t.Errorf("old in-flight save = %v, want ErrLost before reacquisition", err)
	}
	if response := a.await(prompt); response.Error == nil {
		t.Error("queued old prompt crossed insertion after losing ownership")
	}
	select {
	case s := <-opened:
		if err := <-failed; err != nil {
			t.Fatal(err)
		}
		if s == old || !old.isLost() {
			t.Error("obsolete session was reused or left able to save")
		}
		issue50Flush(t, old)
		s.mu.Lock()
		issue50Contains(t, "restored history", s.history, "B retained prompt", "B retained answer")
		s.mu.Unlock()
		// Only the first old save and the new session's repair save may
		// reach Store. Queued flush/claimTurn must see lost first.
		if delta := gate.saves.Load() - before; delta != 2 {
			t.Errorf("Store saves = %d, want one rejected old save and one restore", delta)
		}
		old.Close()
		a.agent.release(old)
		if lost, err := gate.Store.Renew(context.Background(), []string{old.id}); err != nil || len(lost) != 0 {
			t.Errorf("old cleanup released the replacement lease: %v %v", lost, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reacquisition did not finish")
	}
}

func TestIssue50RetakeWaitsForRenewalResult(t *testing.T) {
	a, old, gate, cwd := issue50RaceOwners(t)
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate.releaseRenew) }) }
	defer unblock()
	var releaseSave sync.Once
	unblockSave := func() { releaseSave.Do(func() { close(gate.releaseSave) }) }
	defer unblockSave()
	old.mu.Lock()
	old.history = append(old.history, llm.Message{Role: llm.RoleUser, Content: "stale detached write"})
	old.mu.Unlock()
	gate.saveGate.Store(true)
	flushed := make(chan struct{})
	go func() { old.flush(); close(flushed) }()
	issue50Wait(t, gate.saveEntered)
	gate.renewGate.Store(true)
	renewed := make(chan struct{})
	go func() { a.agent.renew(); close(renewed) }()
	issue50Wait(t, gate.renewEntered)
	response := a.send(acp.MethodSessionResume, map[string]any{"sessionId": old.id, "cwd": cwd})
	a.initialize() // The ownership wait must never block the protocol reader.
	select {
	case <-gate.acquireEntered:
		if result := a.await(response); result.Error != nil {
			t.Fatal(result.Error)
		}
		response = nil
		t.Error("Acquire overtook an unapplied lost renewal result")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	// Renewal may detach the old Session, but must drain its in-flight
	// save before releasing the ownership mutex to a waiting resume.
	select {
	case <-gate.acquireEntered:
		t.Error("reacquisition overtook a detached session's in-flight save")
	case <-time.After(100 * time.Millisecond):
	}
	unblockSave()
	issue50Wait(t, flushed)
	if err := <-gate.saveResult; !errors.Is(err, store.ErrLost) {
		t.Errorf("detached old save = %v, want ErrLost", err)
	}
	issue50Wait(t, renewed)
	if response != nil {
		if result := a.await(response); result.Error != nil {
			t.Fatalf("pending renewal cancelled a later resume: %v", result.Error)
		}
	}
	current, err := a.agent.session(old.id)
	if err != nil || current == old || current.isLost() {
		t.Fatalf("old renewal result dropped the restored session: %v", err)
	}
	current.mu.Lock()
	issue50Contains(t, "restored history", current.history, "B retained prompt", "B retained answer")
	current.mu.Unlock()
}

func TestIssue50OwnershipWaitKeepsOtherTurnsConcurrent(t *testing.T) {
	a, _, gate, cwd := issue50RaceOwners(t)
	id := a.newSession(cwd)
	model := a.agent.deps.model.(scriptedModel)
	model.reply("independent turn finished", llm.FinishStop, nil)
	gate.renewGate.Store(true)
	renewed := make(chan struct{})
	go func() { a.agent.renew(); close(renewed) }()
	issue50Wait(t, gate.renewEntered)
	defer func() { close(gate.releaseRenew); issue50Wait(t, renewed) }()
	if response := a.await(a.prompt(id, "independent turn")); response.Error != nil {
		t.Fatal(response.Error)
	}
	idle := a.untilIdle(id)
	if idle[len(idle)-1].Update.StopReason != string(acp.StopEndTurn) {
		t.Fatalf("unrelated ownership I/O stopped the turn: %+v", idle)
	}
}
