package jsonrpc

import "sync"

// Reply is the answer to a request that a Handler settles later. A handler
// returns a *Reply as its result, keeps a reference, and calls Resolve or
// Reject from wherever the work completes. Only the first settlement counts.
//
// The response is queued for writing synchronously inside Resolve or Reject,
// so a caller that settles a reply and then sends a notification is
// guaranteed to put them on the wire in that order.
type Reply struct {
	mu      sync.Mutex
	settled bool
	result  any
	err     error
	deliver func(result any, err error)
}

// NewReply returns an unsettled reply.
func NewReply() *Reply { return &Reply{} }

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
