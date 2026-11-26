package agent

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// sessionOperation reserves ordering on the ACP reader before work starts.
// Closing done still waits for the predecessor when a queued operation is
// cancelled, so a cancelled prompt cannot let a later resume overtake setup.
type sessionOperation struct {
	ctx      context.Context
	cancel   context.CancelFunc
	previous *sessionOperation
	done     chan struct{}
	reply    requestReply
	syncing  atomic.Bool    // resume or terminal operation still owes session updates
	replay   *historyReplay // released by the operation worker
	prompt   bool
	terminal bool
	err      error // published by closing done
}

// reserveOperation records ACP arrival order before any off-loop work. An
// interrupting close/delete cancels predecessors, but still owns terminal cleanup.
func (a *Agent) reserveOperation(ctx context.Context, id string, terminal, interrupt bool) *sessionOperation {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	context.AfterFunc(ctx, func() { stop() })
	op := &sessionOperation{ctx: ctx, cancel: cancel, done: make(chan struct{}), reply: jsonrpc.NewReply(ctx), terminal: terminal}
	op.syncing.Store(terminal)
	a.mu.Lock()
	defer a.mu.Unlock()
	op.previous = a.operations[id]
	if terminal {
		a.prompts.detach(id)
	}
	if interrupt {
		for prev := op.previous; prev != nil; prev = prev.previous {
			prev.cancel()
		}
	}
	a.operations[id] = op
	a.jobs.Add(1)
	return op
}

// startOperation waits without blocking the reader. Terminal work must still
// release resources after request cancellation; closing done preserves the
// barrier even when a queued operation's reply has already been rejected.
func (a *Agent) startOperation(id string, op *sessionOperation, work func(context.Context) error) {
	go func() {
		defer a.jobs.Done()
		defer func() {
			op.syncing.Store(false)
			a.mu.Lock()
			close(op.done)
			if a.operations[id] == op {
				delete(a.operations, id)
			}
			a.mu.Unlock()
		}()
		if op.previous != nil {
			select {
			case <-op.previous.done:
			case <-op.ctx.Done():
				op.err = acp.ErrRequestCancelled()
				op.reject()
				<-op.previous.done
				if !op.terminal {
					return
				}
			}
		}
		if op.ctx.Err() != nil && !op.terminal {
			op.err = acp.ErrRequestCancelled()
		} else {
			op.err = work(op.ctx)
		}
		if op.err != nil {
			// Setup errors wrap their cause as text, so a store or MCP failure
			// caused by cancellation is recognised by the context instead.
			if errors.Is(op.err, context.Canceled) || op.ctx.Err() != nil {
				op.err = acp.ErrRequestCancelled()
			}
			op.reject()
			op.cancel()
		}
	}()
}

// reject ends an incomplete replay with its error before releasing live events,
// including when a cancelled operation must still wait for its predecessor.
func (op *sessionOperation) reject() {
	op.reply.Reject(op.err)
	if op.replay != nil {
		op.replay.finish(false)
		op.replay = nil
	}
	if !op.terminal { // a cancelled close/delete must still drain the session
		op.syncing.Store(false)
	}
}

// resolveSetup releases replay admission once all updates owed by setup or
// terminal cleanup are ordered, before the peer can act on the response.
func (op *sessionOperation) resolveSetup(result any) {
	op.syncing.Store(false)
	op.reply.Resolve(result)
}

// cancelWaitingPrompts signals cancellation without settling replies owned by
// Session: a store commit racing cancellation may already have crossed insertion.
// Operations still waiting for setup are rejected by startOperation instead.
func (a *Agent) cancelWaitingPrompts(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	tail := a.operations[id]
	for op := tail; op != nil; op = op.previous {
		if op.prompt {
			op.cancel()
		}
	}
	return tail != nil
}
