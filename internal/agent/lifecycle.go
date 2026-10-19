package agent

import (
	"context"
	"errors"

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
	reply    *jsonrpc.Reply
	prompt   bool
	terminal bool
	err      error // published by closing done
}

func (a *Agent) reserveOperation(ctx context.Context, id string, interrupt bool) *sessionOperation {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	context.AfterFunc(ctx, func() { stop() })
	op := &sessionOperation{ctx: ctx, cancel: cancel, done: make(chan struct{}), reply: jsonrpc.NewReply(), terminal: interrupt}
	a.mu.Lock()
	defer a.mu.Unlock()
	op.previous = a.operations[id]
	if interrupt {
		for prev := op.previous; prev != nil; prev = prev.previous {
			prev.cancel()
		}
	}
	a.operations[id] = op
	a.jobs.Add(1)
	return op
}
func (a *Agent) startOperation(id string, op *sessionOperation, work func(context.Context) error) {
	go func() {
		defer a.jobs.Done()
		defer func() {
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
				op.reply.Reject(op.err)
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
			if errors.Is(op.err, context.Canceled) {
				op.err = acp.ErrRequestCancelled()
			}
			op.reply.Reject(op.err)
			op.cancel()
		}
	}()
}
func (a *Agent) cancelWaitingPrompts(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	tail := a.operations[id]
	for op := tail; op != nil; op = op.previous {
		if op.prompt {
			op.cancel()
			op.reply.Reject(acp.ErrRequestCancelled())
		}
	}
	return tail != nil
}
