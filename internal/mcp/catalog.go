package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnly bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

// Tool binds a model-visible alias to one connection and definition revision.
// Copies of a Tool retain that binding across catalog replacement.
type Tool struct {
	Name, Server, OriginalName, Description string
	InputSchema                             json.RawMessage
	ReadOnly                                bool
	ctx                                     context.Context
	cancel                                  context.CancelFunc
	server                                  *server
	definition                              string
}

// Context is cancelled when the connection or this definition is revoked.
func (t Tool) Context() context.Context { return t.ctx }

// Call preserves argument JSON and never retries an operation after delivery.
func (t Tool) Call(ctx context.Context, args string) (Result, error) {
	var result Result
	raw, err := objectArguments(args)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(t.ctx, cancel)
	defer stop()
	if t.ctx.Err() != nil {
		return result, errors.New("MCP tool binding was revoked")
	}
	err = t.server.peer.call(ctx, "tools/call", map[string]any{"name": t.OriginalName, "arguments": raw}, &result)
	if err != nil {
		return result, err
	}
	if result.Content == nil && result.StructuredContent == nil {
		return result, errors.New("MCP result has neither content nor structuredContent")
	}
	return result, nil
}

type server struct {
	cfg               Config
	peer              *peer
	hasTools          bool
	log               *slog.Logger
	mu                sync.Mutex
	tools             []Tool
}

func (s *server) close() { s.peer.fail(errClosed); _ = s.peer.t.Close() }

func connectServer(ctx, lifetime context.Context, cfg Config, cwd string, log *slog.Logger) (*server, error) {
	p := newPeer(lifetime)
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	// Initialization has a setup deadline; the live connection belongs to the
	// session context, not to the completed session/new request.
	stop := context.AfterFunc(ctx, func() { p.fail(ctx.Err()) })
	defer stop()
	var t transport
	var err error
	switch cfg.Type {
	case "stdio":
		t, err = startStdio(p, cfg, cwd)
	default:
		err = errors.New("unsupported MCP transport")
	}
	if err != nil {
		p.cancel()
		return nil, err
	}
	p.t = t
	switch t := t.(type) {
	case *stdioTransport:
		t.start()
	}
	s := &server{cfg: cfg, peer: p, log: log}
	s.hasTools, err = p.initialize(ctx)
	if err == nil {
		err = s.refresh(ctx)
	}
	if err != nil {
		s.close()
		return nil, err
	}
	go s.watch()
	return s, nil
}
func (s *server) watch() {
	for {
		select {
		case <-s.peer.ctx.Done():
			return
		case <-s.peer.changed:
			ctx, cancel := context.WithTimeout(s.peer.ctx, discoveryTimeout)
			if err := s.refresh(ctx); err != nil {
				s.log.Warn("MCP catalog refresh failed", "server", s.cfg.Name, "err", err)
			}
			cancel()
		}
	}
}
func (s *server) refresh(ctx context.Context) error {
	if !s.hasTools {
		return nil
	}
	var all []toolDefinition
	names, cursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	size := 0
	for page := 0; ; page++ {
		if page >= 100 {
			return errors.New("MCP catalog exceeds 100 pages")
		}
		var result struct {
			Tools      []toolDefinition `json:"tools"`
			NextCursor string           `json:"nextCursor"`
		}
		params := map[string]string{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := s.peer.call(ctx, "tools/list", params, &result); err != nil {
			return err
		}
		for _, t := range result.Tools {
			if t.Name == "" || names[t.Name] {
				return errors.New("MCP tool names must be nonempty and unique")
			}
			names[t.Name] = true
			if _, err := objectArguments(string(t.InputSchema)); err != nil || len(t.InputSchema) == 0 {
				return errors.New("MCP inputSchema must be a JSON object")
			}
			size += len(t.InputSchema) + len(t.Description) + len(t.Name)
			if size > maxMessage || len(all) >= 1024 {
				return errors.New("MCP tool catalog exceeds limit")
			}
			all = append(all, t)
		}
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
		if cursors[cursor] {
			return errors.New("MCP tools/list repeated a cursor")
		}
		cursors[cursor] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := map[string]Tool{}
	for _, t := range s.tools {
		old[t.OriginalName] = t
	}
	next := make([]Tool, 0, len(all))
	for _, d := range all {
		raw, _ := jsonrpc.Marshal(d)
		if t, ok := old[d.Name]; ok && t.definition == string(raw) {
			next = append(next, t)
			delete(old, d.Name)
			continue
		}
		ctx, cancel := context.WithCancel(s.peer.ctx)
		next = append(next, Tool{Name: alias(s.cfg.Name, d.Name), Server: s.cfg.Name, OriginalName: d.Name, Description: d.Description, InputSchema: d.InputSchema, ReadOnly: d.Annotations.ReadOnly, ctx: ctx, cancel: cancel, server: s, definition: string(raw)})
	}
	for _, t := range old {
		t.cancel()
	}
	slices.SortFunc(next, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	s.tools = next
	return nil
}

func alias(server, tool string) string {
	raw, _ := jsonrpc.Marshal([]string{server, tool})
	sum := sha256.Sum256(raw)
	return "mcp_" + slug(server, 8) + "_" + slug(tool, 16) + "_" + hex.EncodeToString(sum[:16])
}
func slug(s string, n int) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= n {
			break
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

// Manager owns the servers belonging to one ACP session.
type Manager struct {
	log       *slog.Logger
	resources map[*server]bool
	preparing sync.WaitGroup
	closeDone chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	cwd       string
	mu        sync.Mutex
	servers   []*server
	revision  uint64
	closed    bool
}

// New creates an empty manager; connections are prepared explicitly.
func New(parent context.Context, cwd string, log *slog.Logger) *Manager {
	ctx, cancel := context.WithCancel(parent)
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{ctx: ctx, cancel: cancel, cwd: cwd, log: log, resources: map[*server]bool{}, closeDone: make(chan struct{})}
}

// Change is an unpublished configuration whose new resources must be committed
// or discarded. Existing resources remain usable until publication.
type Change struct {
	manager          *Manager
	ctx              context.Context
	revision         uint64
	servers, created []*server
	settled          bool
}

// Prepare connects new servers without changing the active configuration.
func (m *Manager) Prepare(ctx context.Context, configs []Config) (*Change, error) {
	requestCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m.mu.Lock()
	old := slices.Clone(m.servers)
	revision, closed := m.revision, m.closed
	if !closed {
		m.preparing.Add(1)
	}
	m.mu.Unlock()
	if !closed {
		defer m.preparing.Done()
	}
	if closed || m.ctx.Err() != nil {
		return nil, errClosed
	}
	if len(configs) > 16 {
		return nil, errors.New("at most 16 MCP servers are supported")
	}
	change := &Change{manager: m, ctx: requestCtx, revision: revision, servers: make([]*server, len(configs))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var first error
	sem := make(chan struct{}, 4)
	for _, cfg := range configs {
		if cfg.Type != "stdio" && cfg.Type != "http" {
			return nil, errors.New("unsupported normalized MCP configuration")
		}
	}
	for i, cfg := range configs {
		var found *server
		key, _ := json.Marshal(cfg)
		for _, s := range old {
			other, _ := json.Marshal(s.cfg)
			if string(key) == string(other) && s.peer.ctx.Err() == nil {
				found = s
				break
			}
		}
		if found != nil {
			change.servers[i] = found
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				if first == nil {
					first = ctx.Err()
				}
				mu.Unlock()
				return
			}
			s, err := connectServer(ctx, m.ctx, cfg, m.cwd, m.log)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = fmt.Errorf("MCP server %q: %w", cfg.Name, err)
					cancel()
				}
				return
			}
			m.mu.Lock()
			m.resources[s] = true
			m.mu.Unlock()
			change.servers[i] = s
			change.created = append(change.created, s)
		})
	}
	wg.Wait()
	if first != nil || ctx.Err() != nil {
		change.Discard()
		if first != nil {
			return nil, first
		}
		return nil, ctx.Err()
	}
	seen := map[string]bool{}
	for _, s := range change.servers {
		s.mu.Lock()
		for _, t := range s.tools {
			if seen[t.Name] {
				first = errors.New("MCP tool alias collision")
			}
			seen[t.Name] = true
		}
		s.mu.Unlock()
	}
	if first != nil {
		change.Discard()
		return nil, first
	}
	return change, nil
}

// Commit atomically installs the prepared servers and revokes stale bindings.
func (c *Change) Commit() error {
	m := c.manager
	m.mu.Lock()
	if c.settled || m.closed || m.revision != c.revision || c.ctx.Err() != nil {
		m.mu.Unlock()
		c.Discard()
		return errClosed
	}
	old := m.servers
	m.servers = c.servers
	m.revision++
	c.settled = true
	for _, s := range old {
		if !slices.Contains(c.servers, s) {
			s.peer.fail(errors.New("MCP configuration changed; operation outcome may be unknown"))
			go m.closeResource(s)
		}
	}
	m.mu.Unlock()
	return nil
}

// Discard closes only connections created by this unsuccessful preparation.
func (c *Change) Discard() {
	if c.settled {
		return
	}
	c.settled = true
	var wg sync.WaitGroup
	for _, s := range c.created {
		wg.Go(func() { c.manager.closeResource(s) })
	}
	wg.Wait()
}

// Snapshot returns a stable list of bindings without waiting for network I/O.
func (m *Manager) Snapshot() []Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Tool
	for _, s := range m.servers {
		s.mu.Lock()
		out = append(out, s.tools...)
		s.mu.Unlock()
	}
	slices.SortFunc(out, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Close revokes every binding and releases the session's resources.
func (m *Manager) closeResource(s *server) {
	s.close()
	m.mu.Lock()
	delete(m.resources, s)
	m.mu.Unlock()
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.closeDone
		return
	}
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.preparing.Wait()
	m.mu.Lock()
	servers := make([]*server, 0, len(m.resources))
	for s := range m.resources {
		servers = append(servers, s)
	}
	m.servers = nil
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() { m.closeResource(s) })
	}
	wg.Wait()
	close(m.closeDone)
}

