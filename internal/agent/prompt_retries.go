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
	owner   *pendingPrompts
	id      promptIdentity
	cancel  context.CancelFunc
	waiters []promptWaiter // guarded by owner.mu
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
		g.waiters = append(g.waiters, promptWaiter{ctx, reply, context.AfterFunc(ctx, g.cancel)})
		return nil, g, false
	}
	if p.groups == nil {
		p.groups = make(map[promptIdentity]*promptGroup)
	}
	ctx, cancel := context.WithCancel(ctx)
	g = &promptGroup{owner: p, id: id, cancel: cancel, waiters: []promptWaiter{{ctx: ctx, reply: reply}}}
	p.groups[id] = g
	return ctx, g, true
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

func (g *promptGroup) takeWaiters() []promptWaiter {
	p := g.owner
	p.mu.Lock()
	if p.groups[g.id] == g {
		delete(p.groups, g.id)
	}
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
