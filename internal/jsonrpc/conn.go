package jsonrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
)

// CancelRequestMethod is the protocol-level notification that cancels a
// pending request. Conn handles it itself.
const CancelRequestMethod = "$/cancel_request"

// ErrClosed is returned when sending on a closed connection.
var ErrClosed = errors.New("jsonrpc: connection closed")

// Handler serves one method. Handlers run on the connection's read loop,
// one at a time and in arrival order, so they must not block. A handler that
// has to wait returns NewReply(ctx) and settles it later. The request's reply
// is bound before invocation so work may settle it before the handler returns.
//
// ctx is cancelled when the request is answered, the peer sends $/cancel_request,
// or the connection stops serving. For notifications the result is ignored.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Methods maps method names to handlers.
type Methods map[string]Handler

// Handle adapts a function of decoded params into a Handler; see
// DecodeParams.
func Handle[P any](fn func(context.Context, P) (any, error)) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params P
		if err := DecodeParams(raw, &params); err != nil {
			return nil, err
		}
		return fn(ctx, params)
	}
}

// Conn is a JSON-RPC peer over newline-delimited JSON. All output goes
// through a single writer goroutine, so messages are never interleaved.
type Conn struct {
	in  io.Reader
	out io.Writer
	log *slog.Logger

	queue      chan []byte
	quit       chan struct{}
	closeOnce  sync.Once
	writerDone chan struct{}

	mu       sync.Mutex
	inflight map[ID]*context.CancelFunc // deferred requests by id; the latest wins a duplicate
	calls    map[ID]chan *inbound       // our requests awaiting a response
	lastCall int64
}

// NewConn starts the writer for out; call Close to flush and stop it.
func NewConn(in io.Reader, out io.Writer, log *slog.Logger) *Conn {
	c := &Conn{
		in:         in,
		out:        out,
		log:        log,
		queue:      make(chan []byte, 256),
		quit:       make(chan struct{}),
		writerDone: make(chan struct{}),
		inflight:   make(map[ID]*context.CancelFunc),
		calls:      make(map[ID]chan *inbound),
	}
	go c.writeLoop()
	return c
}

// Notify sends a notification to the peer.
func (c *Conn) Notify(method string, params any) error {
	return c.send(notification{JSONRPC: Version, Method: method, Params: params})
}

// Call sends a request to the peer and decodes the response's result into
// result. It blocks, so it must not be called from a Handler: the response
// arrives through the read loop the handler would be holding up. It returns
// the peer's *Error, ctx's error, or ErrClosed.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	return c.callPeer(ctx, method, params, result, false)
}

// CallCancellable works like Call, but also abandons a blocked enqueue when ctx
// ends and sends a best-effort $/cancel_request for an enqueued pending request.
// Cancellation never waits for output queue space; a delivered response wins.
func (c *Conn) CallCancellable(ctx context.Context, method string, params, result any) error {
	return c.callPeer(ctx, method, params, result, true)
}

func (c *Conn) callPeer(ctx context.Context, method string, params, result any, cancellable bool) error {
	c.mu.Lock()
	c.lastCall++
	id := ID(strconv.FormatInt(c.lastCall, 10))
	done := make(chan *inbound, 1)
	c.calls[id] = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.calls, id)
		c.mu.Unlock()
	}()

	// Permissions retain Call's enqueue semantics; only opt-in requests can
	// abandon an enqueue before there is anything for the peer to cancel.
	sendCtx := context.Background()
	if cancellable {
		sendCtx = ctx
	}
	if err := c.sendContext(sendCtx, request{JSONRPC: Version, ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	var msg *inbound
	select {
	case msg = <-done:
	case <-ctx.Done():
		if !cancellable || c.cancelCall(id) {
			return ctx.Err()
		}
		msg = <-done // deliver claimed the call and buffered its response
	case <-c.quit:
		return ErrClosed
	}
	if msg.failed() {
		var e Error
		if err := json.Unmarshal(msg.Error, &e); err != nil {
			return fmt.Errorf("jsonrpc: decode error of %s: %w", method, err)
		}
		return &e
	}
	if err := json.Unmarshal(msg.Result, result); err != nil {
		return fmt.Errorf("jsonrpc: decode result of %s: %w", method, err)
	}
	return nil
}

// cancelCall competes with deliver under mu. Its caller has already enqueued
// the request, so a cancellation queued here cannot overtake that request.
func (c *Conn) cancelCall(id ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls[id] == nil {
		return false
	}
	delete(c.calls, id)
	select {
	case <-c.quit:
		return true
	default:
	}
	// Locally generated numeric IDs and this fixed payload always marshal.
	b, _ := Marshal(notification{JSONRPC: Version, Method: CancelRequestMethod, Params: struct {
		RequestID ID `json:"requestId"`
	}{id}})
	select {
	case c.queue <- b:
	case <-c.quit:
	default: // peer cancellation is best effort, even with a blocked writer
	}
	return true
}

// deliver hands a response to the Call awaiting it. Responses nobody awaits,
// such as late answers to abandoned calls, are dropped.
func (c *Conn) deliver(msg *inbound) {
	var id ID
	_ = json.Unmarshal(msg.ID, &id)
	c.mu.Lock()
	done := c.calls[id]
	delete(c.calls, id)
	if done != nil {
		done <- msg // buffered before cancellation can observe the removed waiter
	}
	c.mu.Unlock()
	if done == nil {
		c.log.Debug("ignoring response to no pending request", "id", string(msg.ID))
		return
	}
}

// Serve reads and dispatches messages until the input ends (returning nil),
// reading fails, or ctx is done. Contexts of pending requests are cancelled
// on return.
func (c *Conn) Serve(ctx context.Context, methods Methods) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() { readErr <- readLines(c.in, lines, ctx.Done()) }()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return <-readErr
			}
			c.handleLine(ctx, methods, line)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close flushes queued messages and stops the writer. Sending afterwards
// fails with ErrClosed.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() { close(c.quit) })
	<-c.writerDone
	return nil
}

func readLines(r io.Reader, lines chan<- []byte, done <-chan struct{}) error {
	defer close(lines)
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			select {
			case lines <- line:
			case <-done:
				return nil
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (c *Conn) handleLine(ctx context.Context, methods Methods, line []byte) {
	if !json.Valid(line) {
		c.respond(errorResponse(NullID, NewError(CodeParseError, "parse error")))
		return
	}
	if line[0] == '[' {
		c.handleBatch(ctx, methods, line)
		return
	}
	c.dispatch(ctx, methods, line, c.respond)
}

// handleBatch admits every entry in order, then writes one response array
// once all requests in the batch are answered. All-notification batches get
// no response.
func (c *Conn) handleBatch(ctx context.Context, methods Methods, line []byte) {
	var entries []json.RawMessage
	if err := json.Unmarshal(line, &entries); err != nil || len(entries) == 0 {
		c.respond(errorResponse(NullID, NewError(CodeInvalidRequest, "invalid batch")))
		return
	}

	var (
		mu        sync.Mutex
		responses []*response
		pending   sync.WaitGroup
	)
	collect := func(r *response) {
		mu.Lock()
		responses = append(responses, r)
		mu.Unlock()
		pending.Done()
	}
	for _, entry := range entries {
		pending.Add(1)
		if !c.dispatch(ctx, methods, entry, collect) {
			pending.Done()
		}
	}
	go func() {
		pending.Wait()
		if len(responses) > 0 {
			c.sendOrLog(responses)
		}
	}()
}

// dispatch handles one message and reports whether respond will be called
// for it, now or later.
func (c *Conn) dispatch(ctx context.Context, methods Methods, raw []byte, respond func(*response)) bool {
	var msg inbound
	if err := json.Unmarshal(raw, &msg); err != nil {
		respond(errorResponse(NullID, NewError(CodeInvalidRequest, "invalid request: %v", err)))
		return true
	}
	if msg.isResponse() {
		c.deliver(&msg)
		return false
	}

	var id ID
	if msg.ID != nil {
		if err := json.Unmarshal(msg.ID, &id); err != nil {
			respond(errorResponse(NullID, NewError(CodeInvalidRequest, "invalid request: %v", err)))
			return true
		}
	}
	if msg.JSONRPC != Version || msg.Method == "" {
		respond(errorResponse(id, NewError(CodeInvalidRequest, "invalid request: want jsonrpc %q and a method", Version)))
		return true
	}

	if msg.ID == nil {
		c.notify(ctx, methods, msg.Method, msg.Params)
		return false
	}
	c.call(ctx, methods, id, msg.Method, msg.Params, respond)
	return true
}

func (c *Conn) call(ctx context.Context, methods Methods, id ID, method string, params json.RawMessage, respond func(*response)) {
	handler, ok := methods[method]
	if !ok {
		respond(errorResponse(id, NewError(CodeMethodNotFound, "method not found: %s", method)))
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	reply := &Reply{}
	tracked := c.track(id, cancel)
	reply.bind(func(result any, err error) {
		c.untrack(id, tracked)
		cancel()
		respond(newResponse(id, result, err))
	})
	ctx = context.WithValue(ctx, replyContextKey{}, reply)
	result, err := c.invoke(ctx, method, handler, params)
	if deferred, ok := result.(*Reply); ok && err == nil {
		if deferred != reply {
			// Standalone replies can be settled outside a request, then
			// returned by a handler to read their stored result.
			deferred.bind(reply.settle)
		}
		return
	}
	reply.settle(result, err)
}

func (c *Conn) notify(ctx context.Context, methods Methods, method string, params json.RawMessage) {
	if method == CancelRequestMethod {
		c.cancelRequest(params)
		return
	}
	handler, ok := methods[method]
	if !ok {
		c.log.Debug("ignoring unknown notification", "method", method)
		return
	}
	if _, err := c.invoke(ctx, method, handler, params); err != nil {
		c.log.Warn("notification failed", "method", method, "err", err)
	}
}

// invoke runs a handler, turning a panic into an Internal error so one bad
// message cannot take the connection down.
func (c *Conn) invoke(ctx context.Context, method string, handler Handler, params json.RawMessage) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			c.log.Error("handler panicked", "method", method, "panic", p)
			result, err = nil, NewError(CodeInternalError, "internal error")
		}
	}()
	return handler(ctx, params)
}

func (c *Conn) cancelRequest(params json.RawMessage) {
	var p struct {
		RequestID ID `json:"requestId"`
	}
	if err := DecodeParams(params, &p); err != nil || p.RequestID == "" {
		c.log.Warn("malformed cancel request", "params", string(params))
		return
	}
	c.mu.Lock()
	cancel := c.inflight[p.RequestID]
	c.mu.Unlock()
	if cancel != nil {
		(*cancel)()
	}
}

func (c *Conn) track(id ID, cancel context.CancelFunc) *context.CancelFunc {
	c.mu.Lock()
	defer c.mu.Unlock()
	tracked := &cancel
	c.inflight[id] = tracked
	return tracked
}

// untrack removes only its own entry: a client that reuses the id of a
// request still in flight must not make the other one uncancellable.
func (c *Conn) untrack(id ID, tracked *context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[id] == tracked {
		delete(c.inflight, id)
	}
}

func (c *Conn) respond(r *response) { c.sendOrLog(r) }

func (c *Conn) sendOrLog(v any) {
	if err := c.send(v); err != nil {
		c.log.Warn("dropping outgoing message", "err", err)
	}
}

func (c *Conn) send(v any) error {
	return c.sendContext(context.Background(), v)
}

func (c *Conn) sendContext(ctx context.Context, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := Marshal(v)
	if err != nil {
		return fmt.Errorf("jsonrpc: encode message: %w", err)
	}
	select {
	case <-c.quit:
		return ErrClosed
	default:
	}
	select {
	case c.queue <- b:
		return nil
	case <-c.quit:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Conn) writeLoop() {
	defer close(c.writerDone)
	for {
		select {
		case b := <-c.queue:
			c.write(b)
		case <-c.quit:
			for {
				select {
				case b := <-c.queue:
					c.write(b)
				default:
					return
				}
			}
		}
	}
}

func (c *Conn) write(b []byte) {
	if _, err := c.out.Write(append(b, '\n')); err != nil {
		c.log.Error("write failed", "err", err)
	}
}
