// Package mcp relays tools from session-owned MCP servers. It implements a
// deliberately bounded protocol profile; permissions and presentation belong
// to the agent, not to the transport.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

const maxMessage = 8 << 20
const discoveryTimeout = 15 * time.Second

var errClosed = errors.New("MCP connection closed; an in-flight operation may have completed")

// Config is normalized session setup. Credentials are never persisted.
type Config struct {
	Type, Name, Command, URL string
	Args                     []string
	Env, Headers             map[string]string
}

// Result preserves content until the agent chooses its model and ACP projections.
type Result struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

type wire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	message := e.Message
	if len(message) > 8192 {
		message = strings.ToValidUTF8(message[:8192], "�") + " [error truncated]"
	}
	return fmt.Sprintf("MCP error %d: %s", e.Code, message)
}

type transport interface {
	Send(context.Context, []byte) error
	Close() error
	Initialized(string)
}

type peer struct {
	ctx     context.Context
	cancel  context.CancelFunc
	t       transport
	mu      sync.Mutex
	next    uint64
	calls   map[string]chan wire
	failure error
	changed chan struct{}
	replies chan wire
}

func newPeer(parent context.Context) *peer {
	ctx, cancel := context.WithCancel(parent)
	p := &peer{ctx: ctx, cancel: cancel, calls: map[string]chan wire{}, changed: make(chan struct{}, 1), replies: make(chan wire, 16)}
	go func() {
		for {
			select {
			case response := <-p.replies:
				ctx, cancel := context.WithTimeout(p.ctx, time.Second)
				raw, _ := jsonrpc.Marshal(response)
				_ = p.t.Send(ctx, raw)
				cancel()
			case <-p.ctx.Done():
				return
			}
		}
	}()
	return p
}

func (p *peer) fail(err error) {
	p.mu.Lock()
	if p.failure == nil {
		p.failure = err
		p.cancel()
	}
	p.mu.Unlock()
}
func (p *peer) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return p.failure
	}
	return p.ctx.Err()
}
func (p *peer) notify(ctx context.Context, method string, params any) error {
	b, err := jsonrpc.Marshal(params)
	if err != nil {
		return err
	}
	msg, err := jsonrpc.Marshal(wire{JSONRPC: "2.0", Method: method, Params: b})
	if err != nil {
		return err
	}
	return p.t.Send(ctx, msg)
}
func (p *peer) call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.next++
	id := fmt.Sprintf("acplause-%d", p.next)
	done := make(chan wire, 1)
	p.calls[id] = done
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.calls, id); p.mu.Unlock() }()
	rawID, _ := jsonrpc.Marshal(id)
	rawParams, err := jsonrpc.Marshal(params)
	if err != nil {
		return err
	}
	msg, err := jsonrpc.Marshal(wire{JSONRPC: "2.0", ID: rawID, Method: method, Params: rawParams})
	if err != nil {
		return err
	}
	if err = p.t.Send(ctx, msg); err != nil {
		return err
	}
	select {
	case response := <-done:
		if response.Error != nil {
			return response.Error
		}
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("invalid MCP %s result", method)
		}
		return nil
	case <-ctx.Done():
		// A cancelled call is retired before notifying; notification delivery cannot
		// hold up local cancellation or let a late result enter a later turn.
		if method != "initialize" {
			p.mu.Lock()
			delete(p.calls, id)
			p.mu.Unlock()
			go func() {
				c, stop := context.WithTimeout(p.ctx, time.Second)
				defer stop()
				_ = p.notify(c, "notifications/cancelled", map[string]any{"requestId": id, "reason": "caller stopped waiting"})
			}()
		}
		return fmt.Errorf("MCP call stopped: %w; the operation may have completed", ctx.Err())
	case <-p.ctx.Done():
		return p.err()
	}
}
func (p *peer) receive(b []byte) {
	if len(b) > maxMessage {
		p.fail(errors.New("MCP message exceeds 8 MiB"))
		return
	}
	var msg wire
	if json.Unmarshal(b, &msg) != nil || msg.JSONRPC != "2.0" {
		p.fail(errors.New("invalid MCP JSON-RPC message"))
		return
	}
	if msg.Method != "" {
		if msg.ID != nil {
			response := wire{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: -32601, Message: "client method not supported"}}
			if msg.Method == "ping" {
				response.Error = nil
				response.Result = json.RawMessage(`{}`)
			}
			// Bound replies without blocking the transport reader or spawning a
			// goroutine for every unsolicited server request.
			select {
			case p.replies <- response:
			default:
				p.fail(errors.New("too many MCP server requests"))
			}
		} else if msg.Method == "notifications/tools/list_changed" {
			select {
			case p.changed <- struct{}{}:
			default:
			}
		}
		return
	}
	var id string
	if json.Unmarshal(msg.ID, &id) != nil || (msg.Result == nil) == (msg.Error == nil) {
		p.fail(errors.New("invalid MCP response"))
		return
	}
	p.mu.Lock()
	done := p.calls[id]
	delete(p.calls, id)
	p.mu.Unlock()
	if done != nil {
		done <- msg
	}
}

func (p *peer) initialize(ctx context.Context) (bool, error) {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct {
				ListChanged bool `json:"listChanged"`
			} `json:"tools"`
		} `json:"capabilities"`
	}
	err := p.call(ctx, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "acplause", "version": "dev"}}, &result)
	if err != nil {
		return false, err
	}
	if result.ProtocolVersion != "2025-11-25" && result.ProtocolVersion != "2025-06-18" {
		return false, errors.New("unsupported MCP protocol version")
	}
	p.t.Initialized(result.ProtocolVersion)
	if err := p.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return false, err
	}
	return result.Capabilities.Tools != nil, nil
}

func objectArguments(args string) (json.RawMessage, error) {
	raw := bytes.TrimSpace([]byte(args))
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if len(raw) > maxMessage || raw[0] != '{' || !json.Valid(raw) {
		return nil, errors.New("MCP tool arguments must be a JSON object")
	}
	return json.RawMessage(raw), nil
}
