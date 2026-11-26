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

	maxLine int // inbound line bound; a field so tests can shrink it

	ordinary   outputQueue
	cancels    outputQueue
	order      chan *outputQueue // one queue pointer per admitted line, in wire order
	quit       chan struct{}
	writeErr   error // published by closing writerDone
	writerDone chan struct{}

	mu       sync.Mutex
	inflight map[ID]*context.CancelFunc // deferred requests by id; the latest wins a duplicate
	calls    map[ID]pendingCall         // our requests awaiting a response
	lastCall int64
}

// A slot is acquired before admission and released when the writer takes the
// line. For a permission cancellation it is reserved before the request, so
// the cancellation can always be enqueued even if stdout is blocked.
type outputQueue struct {
	lines chan outputFrame
	slots chan struct{}
}

type outputFrame struct {
	line        []byte
	beforeWrite func()
}

func newOutputQueue(capacity int) outputQueue {
	return outputQueue{lines: make(chan outputFrame, capacity), slots: make(chan struct{}, capacity)}
}

type pendingCall struct {
	done     chan *inbound
	reserved bool // owns a slot in cancels until response, cancellation, or shutdown
}

// NewConn starts the writer for out; call Close to flush and stop it.
func NewConn(in io.Reader, out io.Writer, log *slog.Logger) *Conn {
	c := &Conn{
		in:         in,
		out:        out,
		log:        log,
		maxLine:    maxLine,
		ordinary:   newOutputQueue(256),
		cancels:    newOutputQueue(256),
		quit:       make(chan struct{}),
		writerDone: make(chan struct{}),
		inflight:   make(map[ID]*context.CancelFunc),
		calls:      make(map[ID]pendingCall),
	}
	c.order = make(chan *outputQueue, cap(c.ordinary.lines)+cap(c.cancels.lines))
	go c.writeLoop()
	return c
}

// Notify sends a notification to the peer.
func (c *Conn) Notify(method string, params any) error {
	return c.send(notification{JSONRPC: Version, Method: method, Params: params})
}

// CallPermission reserves room for wire cancellation before sending a permission
// request. If ctx ends while awaiting the response, it queues $/cancel_request
// before returning. Only call from a worker: responses arrive on the read loop.
func (c *Conn) CallPermission(ctx context.Context, method string, params, result any) error {
	return c.callPeer(ctx, method, params, result, true)
}

// CallCancellable sends a request with best-effort cancellation, used for
// forms. Both call methods abandon admission if ctx ends before enqueue.
func (c *Conn) CallCancellable(ctx context.Context, method string, params, result any) error {
	return c.callPeer(ctx, method, params, result, false)
}

func (c *Conn) callPeer(ctx context.Context, method string, params, result any, reserve bool) error {
	if reserve {
		if err := c.acquireSlot(ctx, &c.cancels); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.lastCall++
	id := ID(strconv.FormatInt(c.lastCall, 10))
	c.mu.Unlock()
	call := pendingCall{done: make(chan *inbound, 1), reserved: reserve}
	if err := c.sendOrdinary(ctx, request{JSONRPC: Version, ID: id, Method: method, Params: params}, id, call); err != nil {
		if reserve {
			<-c.cancels.slots
		}
		return err
	}
	var msg *inbound
	select {
	case msg = <-call.done:
	case <-ctx.Done():
		if c.cancelCall(id) {
			return ctx.Err()
		}
		msg = <-call.done // delivery or shutdown already claimed the waiter
	}
	if msg == nil {
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
	call, ok := c.calls[id]
	if !ok {
		return false
	}
	delete(c.calls, id)
	queue := &c.cancels
	if !call.reserved {
		queue = &c.ordinary
		select {
		case queue.slots <- struct{}{}:
		default:
			return true // form cancellation never waits for space
		}
	}
	// Locally generated numeric IDs and this fixed payload always marshal.
	b, _ := Marshal(notification{JSONRPC: Version, Method: CancelRequestMethod, Params: struct {
		RequestID ID `json:"requestId"`
	}{id}})
	c.enqueueLocked(queue, outputFrame{line: b})
	return true
}

// deliver hands a response to the call awaiting it. Responses nobody awaits,
// such as late answers to abandoned calls, are dropped.
func (c *Conn) deliver(msg *inbound) {
	var id ID
	_ = json.Unmarshal(msg.ID, &id)
	c.mu.Lock()
	call, ok := c.calls[id]
	if ok {
		c.completeLocked(id, call, msg)
	}
	c.mu.Unlock()
	if !ok {
		c.log.Debug("ignoring response to no pending request", "id", string(msg.ID))
	}
}

// completeLocked releases a waiter's reservation and publishes its response
// (nil on shutdown) before cancellation can observe its removal.
func (c *Conn) completeLocked(id ID, call pendingCall, msg *inbound) {
	delete(c.calls, id)
	if call.reserved {
		<-c.cancels.slots
	}
	call.done <- msg
}

// Serve reads and dispatches messages until the input ends (returning nil),
// the connection closes, reading fails, or ctx is done. Contexts of pending
// requests are cancelled on return. Transport shutdown interrupts a closable
// input so the line reader can exit even when the peer keeps stdin open.
func (c *Conn) Serve(ctx context.Context, methods Methods) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() { readErr <- readLines(c.in, lines, ctx.Done(), c.maxLine) }()

	for {
		select {
		case <-c.quit:
			cancel() // release a line already waiting for dispatch
			if input, ok := c.in.(io.ReadCloser); ok {
				_ = input.Close() // release a line still waiting for input
			}
			return ErrClosed
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

// Close flushes queued messages, stops the writer and returns its first write
// error, if any. Sending afterwards fails with ErrClosed.
func (c *Conn) Close() error {
	c.stop()
	<-c.writerDone
	return c.writeErr
}

// maxLine bounds one inbound line. Image prompts at the ADR 0010 maximum
// (media.MaxSetBytes, base64-encoded) are about 11 MiB, so 64 MiB leaves ample
// room for text and framing while keeping a runaway line from exhausting
// memory. The client is trusted; this is robustness, not a security boundary.
const maxLine = 64 << 20

// readLines sends each non-blank line to lines. A line longer than limit is
// discarded as it streams in, never buffered whole, and reported as a nil
// line (blank lines are otherwise skipped, so nil is unambiguous).
func readLines(r io.Reader, lines chan<- []byte, done <-chan struct{}, limit int) error {
	defer close(lines)
	send := func(line []byte) bool {
		select {
		case lines <- line:
			return true
		case <-done:
			return false
		}
	}
	br := bufio.NewReader(r)
	var buf []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !skipping {
			buf = append(buf, chunk...)
			if len(buf) > limit {
				skipping, buf = true, nil
				if !send(nil) {
					return nil
				}
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if line := bytes.TrimSpace(buf); !skipping && len(line) > 0 {
			if !send(line) {
				return nil
			}
		}
		buf, skipping = nil, false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (c *Conn) handleLine(ctx context.Context, methods Methods, line []byte) {
	if line == nil {
		c.respond(errorResponse(NullID, NewError(CodeParseError, "line too long")))
		return
	}
	if !json.Valid(line) {
		c.respond(errorResponse(NullID, NewError(CodeParseError, "parse error")))
		return
	}
	if line[0] == '[' {
		c.respond(errorResponse(NullID, NewError(CodeInvalidRequest, "ACP does not support JSON-RPC batches")))
		return
	}
	c.dispatch(ctx, methods, line)
}

func (c *Conn) dispatch(ctx context.Context, methods Methods, raw []byte) {
	var msg inbound
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.respond(errorResponse(NullID, NewError(CodeInvalidRequest, "invalid request: %v", err)))
		return
	}
	if msg.isResponse() {
		c.deliver(&msg)
		return
	}

	var id ID
	if msg.ID != nil {
		if err := json.Unmarshal(msg.ID, &id); err != nil {
			c.respond(errorResponse(NullID, NewError(CodeInvalidRequest, "invalid request: %v", err)))
			return
		}
	}
	if msg.JSONRPC != Version || msg.Method == "" {
		c.respond(errorResponse(id, NewError(CodeInvalidRequest, "invalid request: want jsonrpc %q and a method", Version)))
		return
	}

	if msg.ID == nil {
		c.notify(ctx, methods, msg.Method, msg.Params)
		return
	}
	c.call(ctx, methods, id, msg.Method, msg.Params)
}

func (c *Conn) call(ctx context.Context, methods Methods, id ID, method string, params json.RawMessage) {
	handler, ok := methods[method]
	if !ok {
		c.respond(errorResponse(id, NewError(CodeMethodNotFound, "method not found: %s", method)))
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	reply := &Reply{}
	tracked := c.track(id, func() {
		cancel()
		if reply.onCancelRequest != nil {
			reply.onCancelRequest()
		}
	})
	reply.bind(func(result any, err error) {
		c.untrack(id, tracked)
		cancel()
		c.respond(newResponse(id, result, err))
	})
	ctx = context.WithValue(ctx, replyContextKey{}, reply)
	result, err := c.invoke(ctx, method, handler, params)
	if deferred, ok := result.(*Reply); ok && err == nil {
		if deferred == nil {
			c.log.Error("handler returned a nil reply", "method", method)
			reply.settle(nil, NewError(CodeInternalError, "internal error"))
		} else if deferred != reply {
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

func (c *Conn) respond(r *response) {
	if err := c.send(r); err != nil {
		c.log.Warn("dropping outgoing message", "err", err)
	}
}

func (c *Conn) send(v any) error {
	return c.sendOrdinary(context.Background(), v, "", pendingCall{})
}

// acquireSlot waits outside mu. sendOrdinary rechecks cancellation and closure
// at admission because select may choose a slot when both are ready.
func (c *Conn) acquireSlot(ctx context.Context, q *outputQueue) error {
	select {
	case q.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.quit:
		return ErrClosed
	}
}

func (c *Conn) sendOrdinary(ctx context.Context, v any, id ID, call pendingCall) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := Marshal(v)
	if err != nil {
		return fmt.Errorf("jsonrpc: encode message: %w", err)
	}
	frame := outputFrame{line: b}
	if r, ok := v.(*response); ok {
		frame.beforeWrite = r.beforeWrite
	}
	if err := c.acquireSlot(ctx, &c.ordinary); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.quit:
		<-c.ordinary.slots
		return ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		<-c.ordinary.slots
		return err
	}
	if id != "" {
		c.calls[id] = call
	}
	c.enqueueLocked(&c.ordinary, frame)
	return nil
}

// enqueueLocked is the single ordering point. The caller owns a slot in q,
// and order has room for one pointer per slot across both physical FIFOs.
func (c *Conn) enqueueLocked(q *outputQueue, frame outputFrame) {
	q.lines <- frame
	c.order <- q
}

// stop seals admission and releases all response waiters. Producers waiting
// for capacity observe quit; the writer owns slots for already queued lines.
func (c *Conn) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.quit:
		return
	default:
	}
	close(c.quit)
	close(c.order)
	for id, call := range c.calls {
		c.completeLocked(id, call, nil)
	}
}

func (c *Conn) writeLoop() {
	defer close(c.writerDone)
	for q := range c.order {
		frame := <-q.lines
		<-q.slots
		if c.writeErr != nil {
			continue // discard remaining queued lines and release their slots
		}
		b := append(frame.line, '\n')
		if frame.beforeWrite != nil {
			frame.beforeWrite()
		}
		n, err := c.out.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		if err != nil {
			c.writeErr = err
			c.stop()
			c.log.Error("write failed", "err", err)
		}
	}
}
