package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// It governs permission and dispatch waiting, not an admitted call's lifetime.
func (t Tool) Context() context.Context { return t.ctx }

// Call preserves argument JSON and never retries an operation after delivery.
func (t Tool) Call(ctx context.Context, args string) (Result, error) {
	var result Result
	raw, err := ObjectArguments(args)
	if err != nil {
		return result, err
	}
	pending, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(t.ctx, cancel)
	defer stop()
	if t.ctx.Err() != nil {
		return result, errors.New("MCP tool binding was revoked")
	}
	d := &dispatch{pending: pending, admitted: make(chan struct{})}
	d.admit = func() error {
		t.server.mu.Lock()
		defer t.server.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := t.server.peer.ctx.Err(); err != nil {
			return err
		}
		if t.ctx.Err() != nil {
			return errors.New("MCP tool binding was revoked")
		}
		// This is the dispatch linearization point. Refresh can revoke queue
		// waiting, but cannot cancel execution once transport owns the call.
		close(d.admitted)
		return nil
	}
	err = t.server.peer.callDispatch(ctx, "tools/call", map[string]any{"name": t.OriginalName, "arguments": raw}, &result, d)
	if err != nil {
		return result, err
	}
	if result.Content == nil && result.StructuredContent == nil {
		return result, errors.New("MCP result has neither content nor structuredContent")
	}
	return result, nil
}

type server struct {
	recoveryAttempted bool
	cfg               Config
	peer              *peer
	hasTools          bool
	log               *slog.Logger
	mu                sync.Mutex
	tools             []Tool
}

func (s *server) close() { s.peer.fail(errClosed); _ = s.peer.t.Close() }

// Only fixed setup errors may cross into user-visible warnings. Transport and
// protocol errors can contain credentials, endpoint details or arbitrary text.
var (
	errSetupLaunch       = errors.New("could not launch server")
	errSetupInitialize   = errors.New("could not initialize connection")
	errSetupDiscovery    = errors.New("could not discover tools")
	errSetupTimeout      = errors.New("setup timed out")
	errSetupDisconnected = errors.New("connection closed during setup")
)

func connectServer(ctx, lifetime context.Context, cfg Config, cwd string, log *slog.Logger) (*server, error) {
	if ctx.Err() != nil {
		return nil, errSetupTimeout
	}
	p := newPeer(lifetime)
	if p.ctx.Err() != nil {
		p.cancel()
		return nil, errSetupDisconnected
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
	case "http":
		t = newHTTP(p, cfg)
	default:
		err = errors.New("unsupported MCP transport")
	}
	if err != nil {
		p.cancel()
		return nil, errSetupLaunch
	}
	p.t = t
	switch t := t.(type) {
	case *stdioTransport:
		t.log = log
		t.start()
	case *httpTransport:
		t.start()
	}
	s := &server{cfg: cfg, peer: p, log: log}
	failure := errSetupInitialize
	s.hasTools, err = p.initialize(ctx)
	if err == nil {
		failure = errSetupDiscovery
		discovery, cancel := context.WithTimeout(ctx, discoveryTimeout)
		err = s.refresh(discovery)
		cancel()
	}
	stop()
	switch {
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		failure = errSetupTimeout
	case err != nil:
		// Keep the fixed reason for the failed setup step.
	case p.ctx.Err() != nil:
		failure = errSetupDisconnected
	default:
		go s.watch()
		return s, nil
	}
	s.close()
	return nil, failure
}
func (s *server) watch() {
	for {
		select {
		case <-s.peer.ctx.Done():
			return
		case <-s.peer.changed:
			ctx, cancel := context.WithTimeout(s.peer.ctx, discoveryTimeout)
			if err := s.refresh(ctx); err != nil {
				s.log.Warn("MCP catalog refresh failed", "server", s.cfg.Name)
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
			if _, err := ObjectArguments(string(t.InputSchema)); err != nil || len(t.InputSchema) == 0 {
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
	revision  uint64 // desired configuration; HTTP recovery only changes connections
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
	manager  *Manager
	ctx      context.Context
	revision uint64
	entries  []setupEntry
	settled  bool
}

type setupEntry struct {
	server  *server
	config  Config
	failure error
	created bool
}

// SetupFailure identifies a failed peer by its one-based configuration position
// and transport. Reason contains fixed text suitable for a client notice.
type SetupFailure struct {
	Position          int
	Transport, Reason string
}

// Failures reports the omitted entries in input order after a successful commit.
func (c *Change) Failures() []SetupFailure {
	var failures []SetupFailure
	for i, e := range c.entries {
		if e.failure != nil {
			failures = append(failures, SetupFailure{i + 1, e.config.Type, e.failure.Error()})
		}
	}
	return failures
}

// Prepare connects new servers without changing the active configuration.
// Each peer has its own result within the shared concurrency and time budget.
func (m *Manager) Prepare(ctx context.Context, configs []Config) (*Change, error) {
	requestCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
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
	change := &Change{manager: m, ctx: requestCtx, revision: revision, entries: make([]setupEntry, len(configs))}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, cfg := range configs {
		if cfg.Type != "stdio" && cfg.Type != "http" {
			return nil, errors.New("unsupported normalized MCP configuration")
		}
	}
	for i, cfg := range configs {
		entry := &change.entries[i]
		entry.config = cfg
		for _, s := range old {
			if sameConfig(cfg, s.cfg) && s.peer.ctx.Err() == nil {
				entry.server = s
				break
			}
		}
		if entry.server != nil {
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				entry.failure = errSetupTimeout
				return
			}
			s, err := connectServer(ctx, m.ctx, cfg, m.cwd, m.log)
			if err != nil {
				entry.failure = err
				return
			}
			m.mu.Lock()
			m.resources[s] = true
			m.mu.Unlock()
			entry.server, entry.created = s, true
		})
	}
	wg.Wait()
	if err := requestCtx.Err(); err != nil {
		change.Discard()
		return nil, err
	}
	if m.ctx.Err() != nil {
		change.Discard()
		return nil, errClosed
	}
	seen := map[string]bool{}
	for _, e := range change.entries {
		s := e.server
		if s == nil {
			continue
		}
		s.mu.Lock()
		for _, t := range s.tools {
			if seen[t.Name] {
				s.mu.Unlock()
				change.Discard()
				return nil, errors.New("MCP tool alias collision")
			}
			seen[t.Name] = true
		}
		s.mu.Unlock()
	}
	return change, nil
}

// Commit atomically installs the prepared servers and revokes stale bindings.
// A rejected change still needs Discard, outside the caller's session lock.
func (c *Change) Commit() error {
	m := c.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.settled || m.closed || m.revision != c.revision || m.ctx.Err() != nil || c.ctx.Err() != nil {
		return errClosed
	}
	// Recovery may replace a borrowed connection during preparation without
	// changing desired configuration. Keep its current generation, never put
	// the retired connection back or invalidate a valid resume for recovery.
	next := make([]*server, 0, len(c.entries))
	for i := range c.entries {
		e := &c.entries[i]
		s := e.server
		if s == nil || s.peer.ctx.Err() != nil || (!e.created && !slices.Contains(m.servers, s)) {
			for _, current := range m.servers {
				if current.peer.ctx.Err() == nil && sameConfig(current.cfg, e.config) {
					s = current
					break
				}
			}
		}
		if s == nil {
			continue
		}
		if s.peer.ctx.Err() != nil {
			e.failure = errSetupDisconnected
			continue
		}
		e.failure = nil
		next = append(next, s)
	}
	if m.ctx.Err() != nil || c.ctx.Err() != nil {
		return errClosed
	}
	old := m.servers
	m.servers = next
	m.revision++
	c.settled = true
	for _, s := range old {
		if !slices.Contains(next, s) {
			s.peer.fail(errors.New("MCP configuration changed; operation outcome may be unknown"))
			go m.closeResource(s)
		}
	}
	for _, e := range c.entries {
		if e.created && !slices.Contains(next, e.server) {
			go m.closeResource(e.server)
		}
	}
	return nil
}

// Discard closes only connections created by this unsuccessful preparation.
func (c *Change) Discard() {
	if c.settled {
		return
	}
	c.settled = true
	var wg sync.WaitGroup
	for _, e := range c.entries {
		if e.created {
			wg.Go(func() { c.manager.closeResource(e.server) })
		}
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

func (m *Manager) closeResource(s *server) {
	s.close()
	m.mu.Lock()
	delete(m.resources, s)
	m.mu.Unlock()
}

// Close revokes every binding and releases the session's resources.
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

// Recover gives an expired HTTP session one bounded recovery attempt for future
// rounds. Existing bindings remain revoked, even if their aliases reappear.
func (m *Manager) Recover(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	var expired []*server
	for _, s := range m.servers {
		if s.cfg.Type == "http" && !s.recoveryAttempted && errors.Is(s.peer.err(), errSessionExpired) {
			s.recoveryAttempted = true
			expired = append(expired, s)
		}
	}
	if len(expired) == 0 {
		m.mu.Unlock()
		return
	}
	m.preparing.Add(1)
	m.mu.Unlock()
	defer m.preparing.Done()
	for _, old := range expired {
		setup, cancel := context.WithTimeout(ctx, discoveryTimeout)
		next, err := connectServer(setup, m.ctx, old.cfg, m.cwd, m.log)
		cancel()
		if err != nil {
			m.log.Warn("MCP HTTP session recovery failed", "server", old.cfg.Name)
			continue
		}
		m.mu.Lock()
		index := slices.Index(m.servers, old)
		if m.closed || index < 0 || ctx.Err() != nil {
			m.mu.Unlock()
			next.close()
			continue
		}
		m.resources[next] = true
		m.servers[index] = next
		m.mu.Unlock()
		go m.closeResource(old)
	}
}

// sameConfig ignores map iteration order without exposing credential values.
func sameConfig(a, b Config) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
