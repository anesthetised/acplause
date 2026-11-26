package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

const (
	maxReplayFrames       = 64
	maxReplayBytes        = 16 << 20
	maxHeldFrames         = 256
	maxHeldBytes          = 64 << 20
	maxReplayReservations = 16
	replayConflict        = 1001 // acplause: retry after the preceding output obligation
)

var errReplayFull = jsonrpc.NewError(jsonrpc.CodeInternalError, "live updates exceeded replay capacity; retry session/resume")

// replayStreams orders the receipt of a cursor against actual transport
// admission. The lock covers only bookkeeping and already-reserved enqueues.
type replayStreams struct {
	mu                          sync.Mutex
	wire                        *jsonrpc.Conn
	active                      map[string]*historyReplay
	owed                        map[string]int  // cursorless setup or terminal work with updates still due
	frames, bytes, reservations int             // include completion items until dequeue
	armed                       chan struct{}   // wakes ordinary senders waiting for capacity when a cursor arrives
	localTail                   <-chan struct{} // delivery order for in-process Clients
}

func newReplayStreams(out Client) *replayStreams {
	wire, _ := out.(*jsonrpc.Conn)
	return &replayStreams{wire: wire, armed: make(chan struct{})}
}

// outputObligation blocks a new cursor while a setup or closing worker can
// still publish updates. Its last enqueue releases the obligation under mu.
type outputObligation struct {
	streams  *replayStreams
	id       string
	released bool
}

func (rs *replayStreams) owe(id string) *outputObligation {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.owed == nil {
		rs.owed = make(map[string]int)
	}
	rs.owed[id]++
	return &outputObligation{streams: rs, id: id}
}

func (o *outputObligation) releaseLocked() {
	if o == nil || o.released {
		return
	}
	o.released = true
	rs := o.streams
	rs.owed[o.id]--
	if rs.owed[o.id] == 0 {
		delete(rs.owed, o.id)
	}
}

func (o *outputObligation) release() {
	if o != nil {
		o.streams.mu.Lock()
		defer o.streams.mu.Unlock()
		o.releaseLocked()
	}
}

// liveUpdate keeps only encoded output and cut metadata for a wire client.
type liveUpdate struct {
	session              *Session
	line                 []byte
	historical, snapshot bool
	final                bool
	local                acp.Update
	last                 *outputObligation
}

func (s *Session) frame(u acp.Update) (liveUpdate, error) {
	line, err := jsonrpc.NotificationLine(acp.MethodSessionUpdate, acp.SessionNotification{SessionID: s.id, Update: u})
	event := liveUpdate{session: s, line: line, historical: inHistory(u), final: finalUpdate(u)}
	if s.deps.replays.wire == nil {
		event.local = u
	}
	return event, err
}

type historyReplay struct {
	streams *replayStreams
	id      string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	reply   *jsonrpc.Reply
	slot    *jsonrpc.Reservation

	// Guarded by streams.mu. err survives removal so stale workers can
	// return the same failure without sending another response.
	pending []liveUpdate
	bytes   int
	err     error

	// Only the resume worker captures and streams history.
	session *Session
	updates []acp.Update
	cut     int
}

func (rs *replayStreams) begin(ctx context.Context, id string, reply *jsonrpc.Reply) (*historyReplay, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.active[id] != nil || rs.owed[id] != 0 {
		return nil, jsonrpc.NewError(replayConflict, "session synchronization still owes updates; retry session/resume")
	}
	r := &historyReplay{streams: rs, id: id, done: make(chan struct{}), reply: reply}
	if rs.wire != nil && rs.reservations < maxReplayReservations {
		r.slot = rs.wire.TryReserve()
	}
	if rs.reservations >= maxReplayReservations || (rs.wire != nil && r.slot == nil) {
		return nil, jsonrpc.NewError(jsonrpc.CodeInternalError, "no replay completion slot available; retry session/resume")
	}
	r.ctx, r.cancel = context.WithCancel(ctx)
	if rs.active == nil {
		rs.active = make(map[string]*historyReplay)
	}
	rs.active[id] = r
	if reply != nil && rs.wire != nil {
		reply.OnCancelRequest(r.cancelRequest)
	}
	close(rs.armed)
	rs.armed = make(chan struct{})
	rs.reservations++
	return r, nil
}

// cancelRequest orders explicit cancellation on the reader even when setup
// is blocked in storage. The reserved error and live tail need no capacity
// wait. The operation worker still owns cleanup and logical reply settlement.
func (r *historyReplay) cancelRequest() {
	rs := r.streams
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.active[r.id] == r {
		r.err = acp.ErrRequestCancelled()
		r.cancel()
		r.finishLocked(liveUpdate{}, liveUpdate{})
	}
}

// localLocked gives synchronous in-process clients the same delivery order as
// the wire FIFO. The caller runs the returned delivery after releasing mu.
func (rs *replayStreams) localLocked(events []liveUpdate, taken func()) func() error {
	prev := rs.localTail
	done := make(chan struct{})
	rs.localTail = done
	return func() error {
		defer close(done)
		if prev != nil {
			<-prev
		}
		if taken != nil {
			taken()
		}
		for _, e := range events {
			if err := e.session.notify(e.local); err != nil {
				return err
			}
		}
		return nil
	}
}

// admitLive checks twice for an ordinary send: once before waiting for
// capacity, and again around commit. A held event needs no ordinary slot.
func (rs *replayStreams) admitLive(e liveUpdate, commit func() error) (<-chan struct{}, error) {
	rs.mu.Lock()
	if r := rs.active[e.session.id]; r != nil {
		deliver, err := r.holdLocked(e)
		// The cursor now owns this frame. Finishing or cancelling the
		// cursor enqueues its tail under this same lock before another can arm.
		e.last.releaseLocked()
		rs.mu.Unlock()
		if deliver != nil {
			err = deliver()
		}
		return nil, err
	}
	if rs.wire == nil {
		deliver := rs.localLocked([]liveUpdate{e}, nil)
		e.last.releaseLocked()
		rs.mu.Unlock()
		return nil, deliver()
	}
	defer rs.mu.Unlock()
	if commit != nil {
		err := commit()
		e.last.releaseLocked()
		return nil, err
	}
	return rs.armed, nil
}

// holdLocked uses the reserved slot on overflow. The triggering frame is
// the single ordinary payload for that slot, even when it exceeds the byte
// budget. It never enters pending or gets copied into a concatenated buffer.
func (r *historyReplay) holdLocked(e liveUpdate) (func() error, error) {
	rs := r.streams
	n := len(e.line) + 1
	if len(r.pending) >= maxReplayFrames || n > maxReplayBytes-r.bytes || rs.frames >= maxHeldFrames || n > maxHeldBytes-rs.bytes {
		r.err = errReplayFull
		r.cancel()
		return r.finishLocked(liveUpdate{}, e)
	}
	r.pending = append(r.pending, e)
	r.bytes += n
	rs.bytes += n
	rs.frames++
	return nil, nil
}

func (s *Session) live(ctx context.Context, u acp.Update, last *outputObligation) error {
	rs := s.deps.replays
	e, err := s.frame(u)
	if err != nil {
		return err
	}
	e.last = last
	for {
		armed, err := rs.admitLive(e, nil)
		if armed == nil || err != nil {
			return err
		}
		err = rs.wire.SendLine(ctx, e.line, armed, func(commit func() error) error {
			_, err := rs.admitLive(e, commit)
			return err
		})
		if err != jsonrpc.ErrAdmissionChanged {
			return err
		}
	}
}

// snapshot takes the cut under the same lock as transcript mutation and send.
func (r *historyReplay) snapshot(s *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	r.session = s
	r.updates = append([]acp.Update(nil), s.transcript...)
	r.streams.mu.Lock()
	r.cut = len(r.pending)
	r.streams.mu.Unlock()
}

// stageState captures the current state at its position among live updates.
// Failure omits this synthetic update. A stale worker cannot hold it in a
// replacement cursor's stream.
func (r *historyReplay) stageState(s *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.frame(s.state)
	if err != nil {
		return err
	}
	e.snapshot = true
	rs := r.streams
	rs.mu.Lock()
	if err := r.err; err != nil {
		rs.mu.Unlock()
		return err
	}
	deliver, err := r.holdLocked(e)
	failure := r.err
	rs.mu.Unlock()
	if deliver != nil {
		err = deliver()
	}
	return errors.Join(failure, err)
}

func (r *historyReplay) write(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := r.session
	rs := r.streams
	for _, u := range r.updates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if rs.wire == nil {
			if err := s.notify(u); err != nil {
				return err
			}
			continue
		}
		e, err := s.frame(u)
		if err != nil {
			return err
		}
		err = rs.wire.SendLine(ctx, e.line, nil, func(commit func() error) error {
			rs.mu.Lock()
			defer rs.mu.Unlock()
			if r.err != nil {
				return r.err
			}
			if err := s.ctx.Err(); err != nil {
				return err
			}
			return commit()
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// inHistory identifies live patches already represented by a transcript cut.
func inHistory(u acp.Update) bool {
	switch u.(type) {
	case acp.UserMessage, acp.AgentMessage, acp.AgentThought, acp.AgentMessageChunk, acp.AgentThoughtChunk,
		acp.ToolCallUpdate, acp.TerminalUpdate, acp.TerminalOutputChunk, acp.PlanUpdate, acp.PlanRemoved, acp.CompactionUpdate:
		return true
	}
	return false
}

// finalUpdate identifies completions still owed by the retiring worker.
// Its intermediate history patches may describe a different conversation
// than the authoritative transcript loaded by the replacement.
func finalUpdate(u acp.Update) bool {
	switch u := u.(type) {
	case acp.StateUpdate:
		return u.State == acp.StateIdle && u.StopReason != ""
	case acp.ToolCallUpdate:
		return u.Status == acp.ToolCancelled || u.Status == acp.ToolFailed || u.Status == acp.ToolCompleted
	case acp.TerminalUpdate:
		return u.ExitStatus != nil
	case acp.CompactionUpdate:
		return u.Status == acp.CompactionCancelled || u.Status == acp.CompactionFailed || u.Status == acp.CompactionCompleted
	}
	return false
}

// finish commits either the marker or the request error together with all
// owed live lines. A prior overflow already owns its response and reservation.
func (r *historyReplay) finish(ctx context.Context, failure error) error {
	var first liveUpdate
	if failure == nil {
		var err error
		first, err = r.session.frame(acp.ReplayComplete{})
		if err != nil {
			failure = err
		}
	}
	rs := r.streams
	rs.mu.Lock()
	if err := r.err; err != nil {
		rs.mu.Unlock()
		return err
	}
	if failure == nil && (ctx.Err() != nil || r.session.ctx.Err() != nil) {
		failure = acp.ErrRequestCancelled()
	}
	if failure != nil {
		r.err = failure
		first = liveUpdate{}
	}
	deliver, err := r.finishLocked(first, liveUpdate{})
	rs.mu.Unlock()
	if deliver != nil {
		err = deliver()
		if err != nil {
			rs.mu.Lock()
			r.err = err
			rs.mu.Unlock()
		}
	}
	if failure != nil {
		return failure
	}
	return err
}

// finishLocked transfers tail ownership without an intervening enqueue. Only
// the small frame headers are copied. Credits stay charged through dequeue.
func (r *historyReplay) finishLocked(first, trigger liveUpdate) (func() error, error) {
	rs := r.streams
	items := make([]liveUpdate, 0, len(r.pending)+2)
	if first.line != nil {
		items = append(items, first)
	}
	frames, bytes := 0, 0
	for i, e := range r.pending {
		keep := !e.snapshot
		if r.err == nil {
			if e.session == r.session {
				keep = i >= r.cut || !e.historical
			} else {
				keep = e.final
			}
		}
		if keep {
			items = append(items, e)
			frames++
			bytes += len(e.line) + 1
		}
	}
	if trigger.session != nil && !trigger.snapshot {
		items = append(items, trigger)
	}
	delete(rs.active, r.id)
	close(r.done)
	// Covered history patches and abandoned synthetic state were dropped.
	rs.frames -= len(r.pending) - frames
	rs.bytes -= r.bytes - bytes
	r.pending = nil

	release := func() {
		rs.frames -= frames
		rs.bytes -= bytes
		rs.reservations--
	}
	taken := func() { rs.mu.Lock(); defer rs.mu.Unlock(); release() }
	if r.slot == nil {
		return rs.localLocked(items, taken), nil
	}
	lines := make([][]byte, len(items))
	for i, e := range items {
		lines[i] = e.line
	}
	var prefix func() ([]byte, error)
	if r.err != nil && r.reply != nil {
		prefix = r.reply.ErrorLine(r.err)
	}
	err := r.slot.Enqueue(prefix, lines, taken)
	if err != nil {
		release()
		r.err = err
	}
	return nil, err
}

// replayClient checks both at call entry and after acquiring ordinary and
// cancellation capacity. Deferral releases unused reservations before waiting.
type replayClient struct {
	Client
	s *Session
}

type replayWait struct{ done <-chan struct{} }

func (replayWait) Error() string { return "waiting for session replay" }

func (c replayClient) admit(commit func() error) error {
	rs := c.s.deps.replays
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r := rs.active[c.s.id]; r != nil {
		return replayWait{r.done}
	}
	if commit != nil {
		return commit()
	}
	return nil
}

func (c replayClient) call(ctx context.Context, method string, params, result any, call func(context.Context, string, any, any) error) error {
	ctx = jsonrpc.WithAdmission(ctx, c.admit)
	for {
		err := c.admit(nil)
		if err == nil {
			err = call(ctx, method, params, result)
		}
		wait, ok := err.(replayWait)
		if !ok {
			return err
		}
		select {
		case <-wait.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c replayClient) CallPermission(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, c.Client.CallPermission)
}
func (c replayClient) CallCancellable(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, c.Client.CallCancellable)
}
