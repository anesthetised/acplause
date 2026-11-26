package jsonrpc

import (
	"context"
	"sync"
)

// Reply is the answer to a request that a Handler settles later. A handler
// returns a *Reply as its result, keeps a reference, and calls Resolve or
// Reject from wherever the work completes. For a request-bound reply, settlement
// and the handler's return or panic share one completion: only the first counts.
//
// A request-bound reply obtained with NewReply(ctx) queues the response
// synchronously inside Resolve or Reject, even before the handler returns.
// A notification sent afterwards therefore follows the response on the wire.
type Reply struct {
	id              ID
	mu              sync.Mutex
	settled         bool
	result          any
	err             error
	deliver         func(result any, err error)
	onCancelRequest context.CancelFunc // installed and invoked only on the reader
}

type replyContextKey struct{}

// NewReply returns the reply bound to the request in ctx, including derived
// contexts. Without a request binding it returns a standalone reply, whose
// settlement is stored until a handler returns it to a connection.
func NewReply(ctx context.Context) *Reply {
	if reply, ok := ctx.Value(replyContextKey{}).(*Reply); ok {
		return reply
	}
	return &Reply{}
}

// OnCancelRequest registers a synchronous signal for an explicit peer cancellation.
// Call it from the handler before returning. The signal runs on the protocol
// reader, so it must neither wait for application work nor settle the reply.
func (r *Reply) OnCancelRequest(cancel context.CancelFunc) { r.onCancelRequest = cancel }

// Resolve answers the request with result.
func (r *Reply) Resolve(result any) { r.settle(result, nil) }

// Reject answers the request with err; see AsError for the mapping.
func (r *Reply) Reject(err error) { r.settle(nil, err) }

func (r *Reply) settle(result any, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return
	}
	r.settled, r.result, r.err = true, result, err
	if r.deliver != nil {
		r.deliver(result, err)
	}
}

// bind attaches the connection's delivery function, delivering at once if
// the reply was settled before the handler returned.
func (r *Reply) bind(deliver func(any, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliver = deliver
	if r.settled {
		deliver(r.result, r.err)
	}
}

// ErrorLine prepares a failure for a reserved output item. The writer encodes
// even a long request ID outside the application's admission lock.
func (r *Reply) ErrorLine(err error) func() ([]byte, error) {
	response := errorResponse(r.id, AsError(err))
	return func() ([]byte, error) { return Marshal(response) }
}

type sentResponse struct{}

// Delivered settles a request whose response already entered a reserved slot.
func (r *Reply) Delivered() { r.Resolve(sentResponse{}) }
