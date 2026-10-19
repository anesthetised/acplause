// Package agent implements the ACP agent: session methods, the prompt
// lifecycle with its queue and cancellation, and the model/tool loop.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
	"github.com/anesthetised/acplause/internal/tools"
)

// DefaultMaxRounds allows sustained tool work while bounding model requests.
const DefaultMaxRounds = 200

// DefaultMCPTimeout allows long delegated tasks independently of bash.
const DefaultMCPTimeout = time.Hour

// Model streams chat completions; *llm.Client implements it.
type Model interface {
	Chat(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error)
}

// Client is the connection to the ACP client; *jsonrpc.Conn implements it.
type Client interface {
	Notify(method string, params any) error
	// Call sends a request and waits for its result. It blocks, so only
	// session goroutines use it, never handlers on the read loop.
	Call(ctx context.Context, method string, params, result any) error
}

// ListModels loads the model catalog.
type ListModels func(ctx context.Context) ([]llm.Model, error)

// Config configures an Agent. Zero values select defaults.
type Config struct {
	Info       acp.Implementation
	MaxRounds  *int           // nil selects DefaultMaxRounds; zero disables the request limit
	MCPTimeout *time.Duration // nil selects DefaultMCPTimeout; zero disables the tool deadline
	Tools      tools.Options
	Log        *slog.Logger
	Store      Store // required: where sessions are kept
}

const (
	listPageSize   = 50
	catalogTimeout = 15 * time.Second
)

// Agent serves the ACP methods of one connection.
type Agent struct {
	cfg     Config
	deps    deps
	catalog func() (*catalog, error)

	initialized atomic.Bool
	ctx         context.Context // parent of all sessions
	cancel      context.CancelFunc

	mu             sync.Mutex
	sessions       map[string]*Session // open in this process
	lost           map[string]bool     // taken over by another process
	operations     map[string]*sessionOperation
	jobs           sync.WaitGroup
	shuttingDown   bool
	selectionSaved <-chan struct{} // last queued selection write; nil before the first
}

// New creates an agent and starts loading the model catalog.
func New(cfg Config, out Client, model Model, listModels ListModels) *Agent {
	maxRounds := DefaultMaxRounds
	if cfg.MaxRounds != nil {
		maxRounds = *cfg.MaxRounds
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mcpTimeout := DefaultMCPTimeout
	if cfg.MCPTimeout != nil {
		mcpTimeout = *cfg.MCPTimeout
	}
	a := &Agent{
		cfg:        cfg,
		deps:       deps{out: out, model: model, maxRounds: maxRounds, mcpTimeout: mcpTimeout, tools: cfg.Tools, log: cfg.Log, store: cfg.Store},
		ctx:        ctx,
		cancel:     cancel,
		sessions:   make(map[string]*Session),
		lost:       make(map[string]bool),
		operations: make(map[string]*sessionOperation),
	}
	a.catalog = sync.OnceValues(func() (*catalog, error) {
		ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
		defer cancel()
		models, err := listModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot list models: %w", err)
		}
		c := newCatalog(models)
		if len(c.choices) == 0 {
			return nil, errors.New("no models with tool support are available")
		}
		return c, nil
	})
	go a.catalog()
	go a.renewLeases()
	return a
}

// renewLeases keeps this process's sessions its own and drops those another
// process took over (ADR 0004).
func (a *Agent) renewLeases() {
	t := time.NewTicker(store.LeaseRenewal)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		ids := slices.Collect(maps.Keys(a.sessions))
		a.mu.Unlock()
		if len(ids) == 0 {
			continue
		}
		lost, err := a.cfg.Store.Renew(a.ctx, ids)
		if err != nil {
			a.cfg.Log.Warn("renewing session leases", "err", err)
			continue
		}
		for _, id := range lost {
			a.drop(id)
		}
	}
}

// drop forgets a session another process now owns, ending its work here
// without writing to it again.
func (a *Agent) drop(id string) {
	a.mu.Lock()
	s := a.sessions[id]
	delete(a.sessions, id)
	for op := a.operations[id]; op != nil; op = op.previous {
		op.cancel()
	}
	a.lost[id] = true
	a.mu.Unlock()
	if s != nil {
		a.cfg.Log.Info("session taken over by another process", "session", id)
		s.markLost()
		go s.Close()
	}
}

// Methods returns the agent's JSON-RPC handlers.
func (a *Agent) Methods() jsonrpc.Methods {
	return jsonrpc.Methods{
		acp.MethodInitialize:       jsonrpc.Handle(a.initialize),
		acp.MethodSessionNew:       a.ready(jsonrpc.Handle(a.newSession)),
		acp.MethodSessionPrompt:    a.ready(jsonrpc.Handle(a.prompt)),
		acp.MethodSessionCancel:    a.ready(jsonrpc.Handle(a.cancelSession)),
		acp.MethodSessionList:      a.ready(jsonrpc.Handle(a.listSessions)),
		acp.MethodSessionResume:    a.ready(jsonrpc.Handle(a.resumeSession)),
		acp.MethodSessionClose:     a.ready(jsonrpc.Handle(a.closeSession)),
		acp.MethodSessionDelete:    a.ready(jsonrpc.Handle(a.deleteSession)),
		acp.MethodSessionSetConfig: a.ready(jsonrpc.Handle(a.setConfigOption)),
	}
}

// Shutdown cancels all work and closes every session.
func (a *Agent) Shutdown() {
	if saved := a.pendingSelection(); saved != nil {
		<-saved
	}
	a.cancel()
	a.mu.Lock()
	a.shuttingDown = true
	sessions := slices.Collect(maps.Values(a.sessions))
	clear(a.sessions)
	a.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(func() {
			s.Close()
			a.release(s)
		})
	}
	wg.Wait()
	a.jobs.Wait()
}

// ready rejects session methods until initialize has succeeded.
func (a *Agent) ready(h jsonrpc.Handler) jsonrpc.Handler {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if !a.initialized.Load() {
			return nil, jsonrpc.NewError(jsonrpc.CodeInvalidRequest, "connection is not initialized")
		}
		return h(ctx, params)
	}
}

func (a *Agent) initialize(_ context.Context, req acp.InitializeRequest) (any, error) {
	if *req.ProtocolVersion < acp.ProtocolVersion {
		return nil, acp.ErrUnsupportedProtocolVersion(*req.ProtocolVersion)
	}
	a.initialized.Store(true)
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersion,
		Info:            a.cfg.Info,
		Capabilities: acp.AgentCapabilities{
			Session: &acp.SessionCapabilities{
				MCP:                   &acp.MCPCapabilities{Stdio: &acp.Supported{}, HTTP: &acp.Supported{}},
				Prompt:                &acp.PromptCapabilities{EmbeddedContext: &acp.Supported{}, Image: &acp.Supported{}},
				AdditionalDirectories: &acp.Supported{},
				Delete:                &acp.Supported{},
			},
		},
	}, nil
}

// newSession waits for the catalog and earlier model selections off the
// read loop, then creates a session with the last available user choice.
func (a *Agent) newSession(ctx context.Context, req acp.NewSessionRequest) (any, error) {
	reply := jsonrpc.NewReply()
	saved := a.pendingSelection()
	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		if saved != nil {
			select {
			case <-saved:
			case <-ctx.Done():
				reply.Reject(acp.ErrRequestCancelled())
				return
			case <-a.ctx.Done():
				reply.Reject(acp.ErrRequestCancelled())
				return
			}
		}
		c, err := a.catalog()
		if err != nil {
			reply.Reject(turnError(err))
			return
		}
		s, err := newSession(a.ctx, a.deps, c, req.Cwd, req.AdditionalDirectories)
		if err != nil {
			reply.Reject(acp.ErrInvalidParams("cannot open workspace: %v", err))
			return
		}
		change, err := s.mcp.Prepare(ctx, mcpConfigs(req.MCPServers))
		if err == nil {
			err = change.Commit()
			change.Discard()
		}
		if err != nil {
			s.Close()
			reply.Reject(acp.ErrInvalidParams("cannot configure MCP servers: %v", err))
			return
		}
		model, effort, err := a.cfg.Store.Selection(a.ctx)
		if err != nil {
			a.cfg.Log.Warn("loading last model selection", "err", err)
		} else if c.has(model) {
			s.model = model
			if c.reasons(model) && validEffort(effort) {
				s.effort = effort
			}
		}
		if err := a.cfg.Store.Create(a.ctx, s.record()); err != nil {
			s.Close()
			reply.Reject(jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot store the session: %v", err))
			return
		}
		a.mu.Lock()
		if a.shuttingDown || ctx.Err() != nil || a.ctx.Err() != nil {
			a.mu.Unlock()
			s.Close()
			// The client never received this ID. Remove its unpublished record
			// even on shutdown, rather than leaving a listed ghost session.
			if err := a.cfg.Store.Delete(context.WithoutCancel(a.ctx), s.id); err != nil {
				a.cfg.Log.Warn("discarding unpublished session", "session", s.id, "err", err)
				a.release(s)
			}
			reply.Reject(acp.ErrRequestCancelled())
			return
		}
		a.sessions[s.id] = s
		a.mu.Unlock()
		reply.Resolve(acp.NewSessionResponse{SessionID: s.id, ConfigOptions: s.ConfigOptions()})
	}()
	return reply, nil
}

func (a *Agent) prompt(ctx context.Context, req acp.PromptRequest) (any, error) {
	a.mu.Lock()
	pending := a.operations[req.SessionID] != nil
	a.mu.Unlock()
	if !pending {
		s, err := a.session(req.SessionID)
		if err != nil {
			return nil, err
		}
		return s.Submit(ctx, req.Prompt), nil
	}
	op := a.reserveOperation(ctx, req.SessionID, false)
	op.prompt = true
	a.startOperation(req.SessionID, op, func(ctx context.Context) error {
		// A prompt depends on the requested setup, not on whether an earlier
		// prompt was cancelled. Failed setup must reject before insertion.
		for setup := op.previous; setup != nil; setup = setup.previous {
			if !setup.prompt {
				if setup.err != nil {
					return setup.err
				}
				break
			}
		}
		s, err := a.session(req.SessionID)
		if err != nil {
			return err
		}
		s.submit(ctx, req.Prompt, op.reply)
		return nil
	})
	return op.reply, nil
}

func (a *Agent) cancelSession(_ context.Context, n acp.CancelSessionNotification) (any, error) {
	waiting := a.cancelWaitingPrompts(n.SessionID)
	s, err := a.session(n.SessionID)
	if err != nil {
		if waiting {
			return nil, nil
		}
		return nil, err
	}
	s.Cancel()
	return nil, nil
}

// listSessions reads the store, which holds the sessions of every process.
func (a *Agent) listSessions(_ context.Context, req acp.ListSessionsRequest) (any, error) {
	offset, err := decodeCursor(req.Cursor)
	if err != nil {
		return nil, err
	}
	reply := jsonrpc.NewReply()
	go func() {
		sessions, more, err := a.cfg.Store.List(a.ctx, req.Cwd, offset, listPageSize)
		if err != nil {
			reply.Reject(jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot list sessions: %v", err))
			return
		}
		resp := acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}
		for _, sess := range sessions {
			resp.Sessions = append(resp.Sessions, acp.SessionInfo{
				SessionID:             sess.ID,
				Cwd:                   sess.Cwd,
				AdditionalDirectories: sess.Dirs,
				Title:                 sess.Title,
				UpdatedAt:             sess.UpdatedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		if more {
			resp.NextCursor = encodeCursor(offset + len(sessions))
		}
		reply.Resolve(resp)
	}()
	return reply, nil
}

// resumeSession takes the session over in the store, which also renews this
// process's own lease, and loads it unless it is already open here.
func (a *Agent) resumeSession(ctx context.Context, req acp.ResumeSessionRequest) (any, error) {
	replay := req.ReplayFrom != nil
	if replay && req.ReplayFrom.Type != acp.ReplayFromStart {
		return nil, acp.ErrInvalidParams("unsupported replayFrom type %q", req.ReplayFrom.Type)
	}
	op := a.reserveOperation(ctx, req.SessionID, false)
	a.startOperation(req.SessionID, op, func(ctx context.Context) error {
		s, err := a.open(ctx, req.SessionID, req.Cwd)
		if err != nil {
			return err
		}
		if err := s.Resume(ctx, req.AdditionalDirectories, req.MCPServers, replay); err != nil {
			return acp.ErrInvalidParams("cannot configure session: %v", err)
		}
		op.reply.Resolve(acp.ResumeSessionResponse{ConfigOptions: s.ConfigOptions()})
		return nil
	})
	return op.reply, nil
}

// open acquires a session for this process and returns it, loading it from
// the store if it is not open here.
func (a *Agent) open(ctx context.Context, id, cwd string) (*Session, error) {
	l, err := a.cfg.Store.Acquire(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, acp.ErrSessionNotFound(id)
	case errors.Is(err, store.ErrBusy):
		return nil, errSessionBusy()
	case err != nil:
		return nil, jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot load the session: %v", err)
	}
	a.mu.Lock()
	s := a.sessions[id]
	a.mu.Unlock()
	if l.Cwd != cwd {
		if s == nil {
			a.releaseID(id)
		}
		return nil, acp.ErrInvalidParams("cwd %q does not match the session's %q", cwd, l.Cwd)
	}
	if s != nil {
		return s, nil
	}

	c, err := a.catalog()
	if err != nil {
		a.releaseID(id)
		return nil, turnError(err)
	}
	if s, err = restoreSession(a.ctx, a.deps, c, l); err != nil {
		a.releaseID(id)
		return nil, acp.ErrInvalidParams("cannot restore the session: %v", err)
	}
	a.mu.Lock()
	if ctx.Err() != nil || a.shuttingDown || a.ctx.Err() != nil {
		a.mu.Unlock()
		s.Close()
		a.releaseID(id)
		return nil, acp.ErrRequestCancelled()
	}
	if open := a.sessions[id]; open != nil { // a concurrent resume won
		a.mu.Unlock()
		s.Close()
		return open, nil
	}
	a.sessions[id] = s
	delete(a.lost, id)
	a.mu.Unlock()
	s.flush() // store a repair and the idle state
	return s, nil
}

// closeSession cancels the session's work at once, to keep ordering with
// later messages, and answers after the work has wound down and the session
// is released for other processes.
func (a *Agent) closeSession(ctx context.Context, req acp.CloseSessionRequest) (any, error) {
	a.mu.Lock()
	s := a.sessions[req.SessionID]
	// Only pending setup can open an absent session. A close/delete retained
	// until deferred cleanup (or prompts queued after it) cannot reopen it.
	setup := a.operations[req.SessionID]
	for setup != nil && setup.prompt {
		setup = setup.previous
	}
	pending := setup != nil && !setup.terminal
	if pending {
		select {
		case <-setup.done:
			pending = false
		default:
		}
	}
	delete(a.sessions, req.SessionID)
	a.mu.Unlock()
	if s == nil && !pending {
		return nil, acp.ErrSessionNotFound(req.SessionID)
	}
	if s != nil {
		s.Cancel()
	}
	op := a.reserveOperation(ctx, req.SessionID, true)
	a.startOperation(req.SessionID, op, func(context.Context) error {
		s = a.terminalSession(req.SessionID, s)
		if s != nil {
			s.Close()
			a.release(s)
		}
		op.reply.Resolve(acp.CloseSessionResponse{})
		return nil
	})
	return op.reply, nil
}

// deleteSession invalidates local access immediately, then waits for earlier
// setup cleanup before touching the stored record.
func (a *Agent) deleteSession(ctx context.Context, req acp.DeleteSessionRequest) (any, error) {
	a.mu.Lock()
	s := a.sessions[req.SessionID]
	delete(a.sessions, req.SessionID)
	delete(a.lost, req.SessionID)
	a.mu.Unlock()
	if s != nil {
		s.Cancel()
	}
	op := a.reserveOperation(ctx, req.SessionID, true)
	a.startOperation(req.SessionID, op, func(ctx context.Context) error {
		s = a.terminalSession(req.SessionID, s)
		if s != nil {
			s.Close()
		}
		if err := a.cfg.Store.Delete(ctx, req.SessionID); err != nil {
			if s != nil {
				a.release(s)
			}
			if errors.Is(err, store.ErrBusy) {
				return errSessionBusy()
			}
			return jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot delete the session: %v", err)
		}
		op.reply.Resolve(acp.DeleteSessionResponse{})
		return nil
	})
	return op.reply, nil
}

// terminalSession catches a stored resume published between the handler's
// removal and cancellation of earlier setup. Later resumes wait for this op.
// Release/delete the lease only after all local resources have closed.
func (a *Agent) terminalSession(id string, captured *Session) *Session {
	a.mu.Lock()
	late := a.sessions[id]
	delete(a.sessions, id)
	a.mu.Unlock()
	if captured == nil {
		return late
	}
	if late != nil && late != captured {
		late.Close()
	}
	return captured
}

// release gives a closed session's lease back, unless it was lost.
func (a *Agent) release(s *Session) {
	if !s.isLost() {
		a.releaseID(s.id)
	}
}

func (a *Agent) releaseID(id string) {
	if err := a.cfg.Store.Release(context.WithoutCancel(a.ctx), id); err != nil {
		a.cfg.Log.Warn("releasing session", "session", id, "err", err)
	}
}

func (a *Agent) setConfigOption(_ context.Context, req acp.SetConfigOptionRequest) (any, error) {
	s, err := a.session(req.SessionID)
	if err != nil {
		return nil, err
	}
	if err := s.SetConfigOption(req.ConfigID, req.Type, req.Value); err != nil {
		return nil, err
	}
	s.mu.Lock()
	model, effort := s.model, s.effort
	s.mu.Unlock()
	a.rememberSelection(model, effort)
	return acp.SetConfigOptionResponse{ConfigOptions: s.ConfigOptions()}, nil
}

// pendingSelection fences earlier client choices without waiting on the
// read loop. New sessions must see those choices, even before their saves finish.
func (a *Agent) pendingSelection() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selectionSaved
}

// rememberSelection queues writes in request order across all sessions.
// Background session saves and resume never change this user preference.
func (a *Agent) rememberSelection(model, effort string) {
	done := make(chan struct{})
	a.mu.Lock()
	previous := a.selectionSaved
	a.selectionSaved = done
	a.mu.Unlock()
	go func() {
		defer close(done)
		if previous != nil {
			<-previous
		}
		if err := a.cfg.Store.SetSelection(a.ctx, model, effort); err != nil {
			a.cfg.Log.Warn("saving last model selection", "err", err)
		}
	}()
}

func (a *Agent) session(id string) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sessions[id]; ok {
		return s, nil
	}
	if a.lost[id] {
		return nil, errSessionLost()
	}
	return nil, acp.ErrSessionNotFound(id)
}

// Cursors are opaque to clients; inside they are list offsets.
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, acp.ErrInvalidParams("invalid cursor")
	}
	offset, err := strconv.Atoi(string(raw))
	if err != nil || offset < 0 {
		return 0, acp.ErrInvalidParams("invalid cursor")
	}
	return offset, nil
}
