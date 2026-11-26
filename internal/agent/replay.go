package agent

import (
	"context"
	"sync"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// replayStreams belongs to the connection. Keying by wire session ID also
// fences a retiring worker while a resume acquires a replacement generation.
// Its lock covers bookkeeping only, never a session lock or transport I/O.
type replayStreams struct {
	mu     sync.Mutex
	active map[string]*historyReplay
}

type liveUpdate struct {
	session  *Session
	update   acp.Update
	snapshot bool // current-state synchronization belongs only to a completed replay
}

type historyReplay struct {
	streams *replayStreams
	id      string
	done    chan struct{}
	pending []liveUpdate // guarded by streams.mu

	// Only the resume worker takes or emits the snapshot.
	session *Session
	updates []acp.Update
	cut     int
}

func (rs *replayStreams) begin(id string) *historyReplay {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.active[id] != nil {
		return nil
	}
	if rs.active == nil {
		rs.active = make(map[string]*historyReplay)
	}
	r := &historyReplay{streams: rs, id: id, done: make(chan struct{})}
	rs.active[id] = r
	return r
}

func (rs *replayStreams) hold(event liveUpdate) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r := rs.active[event.session.id]; r != nil {
		r.pending = append(r.pending, event)
		return true
	}
	return false
}

// snapshot takes the cut under the same lock as transcript mutation and send.
// A restored generation replaces any snapshot of a previous owner.
func (r *historyReplay) snapshot(s *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.session = s
	r.updates = make([]acp.Update, len(s.transcript)+1)
	copy(r.updates, s.transcript)
	r.updates[len(s.transcript)] = acp.ReplayComplete{}
	r.streams.mu.Lock()
	r.cut = len(r.pending)
	r.streams.mu.Unlock()
}

func (r *historyReplay) write(ctx context.Context) error {
	s := r.session
	for _, u := range r.updates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if err := s.notify(u); err != nil {
			return jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot deliver session replay: %v", err)
		}
	}
	return nil
}

// inHistory identifies live patches whose cumulative effect is already in a
// transcript snapshot. Live-only updates (including future kinds) pass through.
func inHistory(u acp.Update) bool {
	switch u.(type) {
	case acp.UserMessage, acp.AgentMessage, acp.AgentThought, acp.AgentMessageChunk, acp.AgentThoughtChunk,
		acp.ToolCallUpdate, acp.TerminalUpdate, acp.TerminalOutputChunk, acp.PlanUpdate, acp.CompactionUpdate:
		return true
	}
	return false
}

// finish drains in delivery order before reopening direct sends. After an
// error, even patches that would have been covered by the cut are owed live.
func (r *historyReplay) finish(complete bool) (firstErr error) {
	rs := r.streams
	index := 0
	for {
		rs.mu.Lock()
		batch := r.pending
		r.pending = nil
		if len(batch) == 0 {
			delete(rs.active, r.id)
			close(r.done)
			rs.mu.Unlock()
			return firstErr
		}
		rs.mu.Unlock()
		for _, event := range batch {
			deliver := !event.snapshot
			if complete {
				deliver = event.session == r.session && (index >= r.cut || !inHistory(event.update))
			}
			if deliver {
				if err := event.session.notify(event.update); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			index++
		}
	}
}

// replayClient delays newly issued session requests alongside their live
// tool and state updates. A request already in flight keeps its original ID.
type replayClient struct {
	Client
	s *Session
}

func (c replayClient) wait(ctx context.Context) error {
	rs := c.s.deps.replays
	for {
		rs.mu.Lock()
		r := rs.active[c.s.id]
		rs.mu.Unlock()
		if r == nil {
			return ctx.Err()
		}
		select {
		case <-r.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c replayClient) CallPermission(ctx context.Context, method string, params, result any) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.Client.CallPermission(ctx, method, params, result)
}

func (c replayClient) CallCancellable(ctx context.Context, method string, params, result any) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.Client.CallCancellable(ctx, method, params, result)
}
