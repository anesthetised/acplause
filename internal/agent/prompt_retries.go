package agent

import (
	"context"
	"maps"
	"sync"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// requestReply lets a single insertion settle either one request or all of its retries.
type requestReply interface {
	Resolve(any)
	Reject(error)
}

type promptIdentity struct {
	session, key string
}

// pendingPrompts spans both admission paths. The reader registers attempts
// before setup or a session worker can claim them. Its lock never covers I/O.
type pendingPrompts struct {
	mu     sync.Mutex
	groups map[promptIdentity]*promptGroup
}

type promptGroup struct {
	owner      *pendingPrompts
	id         promptIdentity
	cancel     context.CancelFunc
	cancelSave context.CancelFunc // installed before insertion, guarded by owner.mu
	waiters    []promptWaiter     // guarded by owner.mu
}

type promptWaiter struct {
	ctx   context.Context
	reply *jsonrpc.Reply
	stop  func() bool
}

// join returns the shared insertion context to the first attempt only.
// Every request retains its own connection-bound reply.
func (p *pendingPrompts) join(ctx context.Context, id promptIdentity, reply *jsonrpc.Reply) (context.Context, *promptGroup, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g := p.groups[id]
	if g != nil {
		reply.OnCancelRequest(g.cancelRequest)
		g.waiters = append(g.waiters, promptWaiter{ctx, reply, context.AfterFunc(ctx, g.cancel)})
		return nil, g, false
	}
	if p.groups == nil {
		p.groups = make(map[promptIdentity]*promptGroup)
	}
	shared, cancel := context.WithCancel(ctx)
	g = &promptGroup{owner: p, id: id, cancel: cancel, waiters: []promptWaiter{{ctx: ctx, reply: reply}}}
	p.groups[id] = g
	reply.OnCancelRequest(g.cancelRequest)
	return shared, g, true
}

// cancelRequest reaches the current save before the reader handles the next
// message. Only the worker can tell whether that save has already committed.
func (g *promptGroup) cancelRequest() {
	g.cancel()
	g.owner.mu.Lock()
	defer g.owner.mu.Unlock()
	if g.cancelSave != nil {
		g.cancelSave()
	}
}

// bindSave installs the in-flight signal. An earlier cancellation is visible
// to the worker's admission checks through the group context.
func (g *promptGroup) bindSave(cancel context.CancelFunc) {
	g.owner.mu.Lock()
	defer g.owner.mu.Unlock()
	g.cancelSave = cancel
}

// detach fences a terminal operation. New requests must pass through it.
// The old worker still settles its own waiters even if its commit won the race.
func (p *pendingPrompts) detach(session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	maps.DeleteFunc(p.groups, func(id promptIdentity, _ *promptGroup) bool { return id.session == session })
}

// cancelled also sees cancellation before a waiter's AfterFunc has run.
func (g *promptGroup) cancelled() bool {
	g.owner.mu.Lock()
	defer g.owner.mu.Unlock()
	for _, w := range g.waiters {
		if w.ctx.Err() != nil {
			return true
		}
	}
	return false
}

// detachLocked ends admission to this group without disturbing a newer group
// for the same key. The current waiters still receive this group's result.
func (g *promptGroup) detachLocked() {
	if g.owner.groups[g.id] == g {
		delete(g.owner.groups, g.id)
	}
}

func (g *promptGroup) takeWaiters() []promptWaiter {
	p := g.owner
	p.mu.Lock()
	g.detachLocked()
	waiters := g.waiters
	g.waiters = nil
	p.mu.Unlock()
	for _, w := range waiters {
		if w.stop != nil {
			w.stop()
		}
	}
	g.cancel()
	return waiters
}

func (g *promptGroup) Resolve(result any) {
	for _, w := range g.takeWaiters() {
		w.reply.Resolve(result)
	}
}

func (g *promptGroup) Reject(err error) {
	for _, w := range g.takeWaiters() {
		w.reply.Reject(err)
	}
}
