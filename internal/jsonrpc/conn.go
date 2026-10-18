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
	"sync"
)

// CancelRequestMethod is the protocol-level notification that cancels a
// pending request. Conn handles it itself.
const CancelRequestMethod = "$/cancel_request"

// ErrClosed is returned when sending on a closed connection.
var ErrClosed = errors.New("jsonrpc: connection closed")

// Handler serves one method. Handlers run on the connection's read loop,
// one at a time and in arrival order, so they must not block. A handler that
// has to wait returns a *Reply and settles it later.
//
// ctx is cancelled when the peer sends $/cancel_request for the request or
// the connection stops serving. For notifications the result is ignored.
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
	inflight map[ID]context.CancelFunc // deferred requests by id
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
		inflight:   make(map[ID]context.CancelFunc),
	}
	go c.writeLoop()
	return c
}

// Notify sends a notification to the peer.
func (c *Conn) Notify(method string, params any) error {
	return c.send(notification{JSONRPC: Version, Method: method, Params: params})
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
		c.log.Debug("ignoring response from peer", "id", string(msg.ID))
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
	result, err := c.invoke(ctx, method, handler, params)
	reply, deferred := result.(*Reply)
	if !deferred || err != nil {
		cancel()
		respond(newResponse(id, result, err))
		return
	}

	c.track(id, cancel)
	reply.bind(func(result any, err error) {
		c.untrack(id)
		cancel()
		respond(newResponse(id, result, err))
	})
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
		cancel()
	}
}

func (c *Conn) track(id ID, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight[id] = cancel
}

func (c *Conn) untrack(id ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, id)
}

func (c *Conn) respond(r *response) { c.sendOrLog(r) }

func (c *Conn) sendOrLog(v any) {
	if err := c.send(v); err != nil {
		c.log.Warn("dropping outgoing message", "err", err)
	}
}

func (c *Conn) send(v any) error {
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
