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
	// CallPermission reserves wire cancellation for a pending permission.
	// Both calls block and belong on session workers.
	CallPermission(ctx context.Context, method string, params, result any) error
	// CallCancellable sends best-effort wire cancellation for forms.
	CallCancellable(ctx context.Context, method string, params, result any) error
}

// ListModels loads the model catalog.
type ListModels func(ctx context.Context) ([]llm.Model, error)

// Config configures an Agent. Zero values select defaults.
type Config struct {
	ReadConcurrency int // built-in read workers; zero selects 1, bounded to 1..8
	Info            acp.Implementation
	DefaultModel    string         // launcher fallback when a saved choice is unavailable
	MaxRounds       *int           // nil selects DefaultMaxRounds; zero disables the request limit
	MCPTimeout      *time.Duration // nil selects DefaultMCPTimeout; zero disables the tool deadline
	TurnTimeout     *time.Duration // nil or zero disables the post-insertion turn deadline
	MaxTurnTokens   uint64         // zero disables the reported input+output token limit
	MaxTurnCostUSD  float64        // zero disables the reported USD limit
	Tools           tools.Options
	Log             *slog.Logger
	Store           Store // required: where sessions are kept
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

	// Connection admission state belongs to the JSON-RPC read loop.
	initialized      bool
	otherRequestSeen bool
	ctx              context.Context // parent of all sessions
	cancel           context.CancelFunc

	// ownershipMu serialises acquisition/publication with renewal and its
	// loss results. Only background operations take it; turns use their own
	// session locks. Take it before a session's flushMu, never under mu.
	ownershipMu sync.Mutex
	prompts     pendingPrompts

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
	var turnTimeout time.Duration
	if cfg.TurnTimeout != nil {
		turnTimeout = *cfg.TurnTimeout
	}
	a := &Agent{
		cfg:        cfg,
		deps:       deps{replays: newReplayStreams(out), forms: new(atomic.Bool), out: out, model: model, maxRounds: maxRounds, readConcurrency: cfg.ReadConcurrency, mcpTimeout: mcpTimeout, turnTimeout: turnTimeout, maxTurnTokens: cfg.MaxTurnTokens, maxTurnCostUSD: cfg.MaxTurnCostUSD, tools: cfg.Tools, log: cfg.Log, store: cfg.Store, calibration: new(calibration)},
		ctx:        ctx,
		cancel:     cancel,
		sessions:   make(map[string]*Session),
		lost:       make(map[string]bool),
		operations: make(map[string]*sessionOperation),
	}
	a.catalog = cacheCatalog(func() (*catalog, error) {
		ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
		defer cancel()
		models, err := listModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot list models: %w", err)
		}
		c := newCatalog(models)
		if len(c.choices) == 0 {
			return nil, errors.New("no models with tool support are available. Configure a provider that offers one before setting ACPLAUSE_DEFAULT_MODEL")
		}
		return c, nil
	}, catalogTTL, cfg.Log)
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
		a.renew()
	}
}

// renew applies one ownership check, including its loss notifications.
func (a *Agent) renew() {
	a.ownershipMu.Lock()
	defer a.ownershipMu.Unlock()
	a.mu.Lock()
	ids := make([]string, 0, len(a.sessions))
	pending := make(map[string]*sessionOperation, len(a.sessions))
	for id := range a.sessions {
		if a.lost[id] {
			continue // renewal already retired this generation
		}
		ids = append(ids, id)
		pending[id] = a.operations[id]
	}
	a.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	lost, err := a.cfg.Store.Renew(a.ctx, ids)
	if err != nil {
		a.cfg.Log.Warn("renewing session leases", "err", err)
		return
	}
	for _, id := range lost {
		a.dropLocked(id, pending[id])
	}
}

// dropLocked applies a loss to the session and setup captured by its check.
// Requires ownershipMu. A later resume may already be waiting for that lock;
// cancelling its reservation would prevent it from restoring the session.
func (a *Agent) dropLocked(id string, pending *sessionOperation) {
	a.mu.Lock()
	s := a.sessions[id]
	if s != nil {
		s.activation.Store(activationLost)
		a.jobs.Add(1)
	}
	a.prompts.detach(id)
	for op := pending; op != nil; op = op.previous {
		op.cancel()
	}
	a.lost[id] = true
	a.mu.Unlock()
	if s != nil {
		a.cfg.Log.Info("session taken over by another process", "session", id)
		s.markLost()
		// A terminal handler can detach s before acquire sees it. Fence any
		// store write already in flight before releasing ownershipMu (ADR 0004).
		s.flushMu.Lock()
		// Keep the lost generation reachable until its final updates are queued.
		// Resume and close can join this cleanup without holding an agent lock.
		go func() {
			defer a.jobs.Done()
			s.Close()
			a.mu.Lock()
			if a.sessions[id] == s {
				a.detachSessionLocked(id)
			}
			a.mu.Unlock()
		}()
		s.flushMu.Unlock()
	}
}

// Methods returns the agent's JSON-RPC handlers.
func (a *Agent) Methods() jsonrpc.Methods {
	return jsonrpc.Methods{
		BeforeRequest:      a.beforeRequest,
		AcceptNotification: func() bool { return a.initialized },
		Requests: map[string]jsonrpc.Handler{
			acp.MethodInitialize:       a.initialize,
			acp.MethodSessionNew:       jsonrpc.Handle(a.newSession),
			acp.MethodSessionPrompt:    jsonrpc.Handle(a.prompt),
			acp.MethodSessionList:      jsonrpc.Handle(a.listSessions),
			acp.MethodSessionResume:    jsonrpc.Handle(a.resumeSession),
			acp.MethodSessionClose:     jsonrpc.Handle(a.closeSession),
			acp.MethodSessionDelete:    jsonrpc.Handle(a.deleteSession),
			acp.MethodSessionSetConfig: jsonrpc.Handle(a.setConfigOption),
		},
		Notifications: map[string]jsonrpc.Handler{
			acp.MethodSessionCancel: jsonrpc.Handle(a.cancelSession),
		},
	}
}

// Shutdown cancels all work and closes every session.
func (a *Agent) Shutdown() {
	a.cancel()
	a.mu.Lock()
	a.shuttingDown = true
	sessions := slices.Collect(maps.Values(a.sessions))
	for _, s := range sessions {
		s.detachActivation()
	}
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
	if saved := a.pendingSelection(); saved != nil {
		<-saved
	}
}

// beforeRequest enforces the handshake even for requests that fail dispatch
// or params validation. Failed initializations can still be retried.
func (a *Agent) beforeRequest(method string) error {
	if method == acp.MethodInitialize {
		if a.initialized && a.otherRequestSeen {
			return jsonrpc.NewError(jsonrpc.CodeInvalidRequest, "initialize cannot be repeated after another request")
		}
		return nil
	}
	a.otherRequestSeen = true
	if !a.initialized {
		return jsonrpc.NewError(jsonrpc.CodeInvalidRequest, "connection is not initialized")
	}
	return nil
}

func (a *Agent) initialize(_ context.Context, params json.RawMessage) (any, error) {
	capabilities, err := acp.DecodeInitializeRequest(params)
	if err != nil {
		return nil, err
	}
	result := acp.InitializeResponse{
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
	}
	return jsonrpc.OnEnqueue(result, func() {
		a.deps.forms.Store(capabilities.Elicitation != nil && capabilities.Elicitation.Form != nil)
		a.initialized = true
	}), nil
}

// newSession waits for the catalog and earlier model selections off the
// read loop. Setup needs a usable model even if the client has no selector.
func (a *Agent) newSession(ctx context.Context, req acp.NewSessionRequest) (any, error) {
	reply := jsonrpc.NewReply(ctx)
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
		if ctx.Err() != nil || a.ctx.Err() != nil {
			reply.Reject(acp.ErrRequestCancelled())
			return
		}
		if err != nil {
			reply.Reject(turnError(err))
			return
		}
		model, effort, err := a.cfg.Store.Selection(ctx)
		if ctx.Err() != nil || a.ctx.Err() != nil {
			reply.Reject(acp.ErrRequestCancelled())
			return
		}
		if err != nil {
			a.cfg.Log.Warn("loading last model selection", "err", err)
			model, effort = "", ""
		}
		choice, err := c.initialSelection(modelSelection{model, effort}, a.cfg.DefaultModel)
		if err != nil {
			reply.Reject(turnError(err))
			return
		}
		s, err := newSession(a.ctx, a.deps, c, req.Cwd, req.AdditionalDirectories)
		if err != nil {
			reply.Reject(acp.ErrInvalidParams("cannot open workspace: %v", err))
			return
		}
		s.model, s.effort = choice.model, choice.effort
		change, err := s.mcp.Prepare(ctx, mcpConfigs(req.MCPServers))
		if err == nil {
			err = change.Commit()
			change.Discard()
		}
		if err != nil || ctx.Err() != nil || a.ctx.Err() != nil {
			s.Close()
			if ctx.Err() != nil || a.ctx.Err() != nil {
				reply.Reject(acp.ErrRequestCancelled())
			} else {
				reply.Reject(acp.ErrInvalidParams("cannot configure MCP servers: %v", err))
			}
			return
		}
		if err := a.cfg.Store.Create(a.ctx, s.record()); err != nil {
			s.Close()
			reply.Reject(jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot store the session: %v", err))
			return
		}
		failures := change.Failures()
		var owed *outputObligation
		if len(failures) != 0 {
			owed = a.deps.replays.owe(s.id)
			defer owed.release()
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
		if owed != nil {
			s.setupOutput.Add(1)
			defer s.setupOutput.Done()
		}
		a.sessions[s.id] = s
		a.mu.Unlock()
		reply.Resolve(acp.NewSessionResponse{SessionID: s.id, ConfigOptions: s.ConfigOptions(), AvailableCommands: availableCommands()})
		s.reportMCPSetup(a.ctx, failures, len(req.MCPServers), owed)
	}()
	return reply, nil
}

// rejectLater keeps the reader available even when ordinary output is full.
func (a *Agent) rejectLater(reply *jsonrpc.Reply, err error) (any, error) {
	a.jobs.Go(func() { reply.Reject(err) })
	return reply, nil
}

func (a *Agent) prompt(ctx context.Context, req acp.PromptRequest) (any, error) {
	reply := jsonrpc.NewReply(ctx)
	sub := &submission{ctx: ctx, prompt: req.Prompt, key: string(req.IdempotencyKey), reply: reply}
	a.mu.Lock()
	previous := a.operations[req.SessionID]
	s := a.sessions[req.SessionID]
	// Admission belongs to the activation visible at receipt. Register the
	// key under the same fence as detachment on close or ownership loss.
	activation := activationDetached
	if s != nil {
		activation = s.activation.Load()
	}
	writing := activation == activationWriting
	staged := writing || (previous != nil && previous.staged)
	if (activation != activationActive && !writing) || (previous != nil && previous.terminal) {
		lost := previous == nil && (activation == activationLost || a.lost[req.SessionID])
		a.mu.Unlock()
		if lost {
			return a.rejectLater(reply, errSessionLost())
		}
		return a.rejectLater(reply, acp.ErrSessionNotFound(req.SessionID))
	}
	// A setter or another prompt can keep a completed setup in the chain.
	// Only setup still in progress at receipt can fail this prompt.
	setup := previous
	for setup != nil && (setup.prompt || setup.config) {
		setup = setup.previous
	}
	if !setup.pending() {
		setup = nil
	}
	if sub.key != "" {
		shared, group, first := a.prompts.join(ctx, promptIdentity{req.SessionID, sub.key}, reply)
		if !first {
			a.mu.Unlock()
			return reply, nil
		}
		sub.ctx, sub.reply = shared, group
		if staged {
			group.setStaged(true)
		}
	}
	a.mu.Unlock()
	if previous == nil && !staged {
		s.submit(sub, nil)
		return reply, nil
	}
	op := a.reserveOperation(sub.ctx, req.SessionID, false, false)
	op.prompt = true
	op.staged = staged
	op.reply = sub.reply
	sub.ctx = op.ctx
	a.startOperation(req.SessionID, op, func(ctx context.Context) error {
		if writing {
			if err := s.awaitWritten(ctx); err != nil {
				return err
			}
		}
		var setupErr error
		if setup != nil {
			setupErr = setup.err
		}
		if err := a.checkGeneration(s); err != nil {
			return err
		}
		s.submit(sub, setupErr)
		return nil
	})
	return reply, nil
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
func (a *Agent) listSessions(ctx context.Context, req acp.ListSessionsRequest) (any, error) {
	offset, err := decodeCursor(req.Cursor)
	if err != nil {
		return nil, err
	}
	reply := jsonrpc.NewReply(ctx)
	a.jobs.Go(func() { // Shutdown waits for it: the store closes after Shutdown
		sessions, more, err := a.cfg.Store.List(a.ctx, req.Cwd, offset, listPageSize)
		if err != nil {
			reply.Reject(jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot list sessions: %v", err))
			return
		}
		resp := acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}
		for _, sess := range sessions {
			info := sessionInfoPatch(sess.Title, sess.UpdatedAt)
			resp.Sessions = append(resp.Sessions, acp.SessionInfo{
				SessionID:             sess.ID,
				Cwd:                   sess.Cwd,
				AdditionalDirectories: sess.Dirs,
				Title:                 info.Title,
				UpdatedAt:             info.UpdatedAt,
			})
		}
		if more {
			resp.NextCursor = encodeCursor(offset + len(sessions))
		}
		reply.Resolve(resp)
	})
	return reply, nil
}

// resumeSession takes the session over in the store, which also renews this
// process's own lease, and restores it unless local ownership was continuous.
func (a *Agent) resumeSession(ctx context.Context, req acp.ResumeSessionRequest) (any, error) {
	replay := req.ReplayFrom != nil
	if replay && req.ReplayFrom.Type != acp.ReplayFromStart {
		return nil, acp.ErrInvalidParams("unsupported replayFrom type %q", req.ReplayFrom.Type)
	}
	var r *historyReplay
	reply := jsonrpc.NewReply(ctx)
	if replay {
		var err error
		r, err = a.deps.replays.begin(ctx, req.SessionID, reply)
		if err != nil {
			return a.rejectLater(reply, err)
		}
		ctx = r.ctx
	}
	a.mu.Lock()
	live := a.sessions[req.SessionID]
	a.mu.Unlock()
	op := a.reserveOperation(ctx, req.SessionID, false, false)
	if !replay {
		op.owed = a.deps.replays.owe(req.SessionID)
	}
	op.replay = r
	a.startOperation(req.SessionID, op, func(ctx context.Context) error {
		if op.replay != nil && live != nil {
			op.replay.snapshot(live)
		}
		return a.resume(ctx, req, op)
	})
	return op.reply, nil
}

// resume keeps a restored session private until setup succeeds.
func (a *Agent) resume(ctx context.Context, req acp.ResumeSessionRequest, op *sessionOperation) (err error) {
	s, restored, err := a.open(ctx, req.SessionID, req.Cwd, req.AdditionalDirectories)
	if err != nil {
		return err
	}
	if restored {
		defer func() {
			if err != nil {
				s.Close()
				a.release(s)
			}
		}()
	}
	var guard func(func() error) error
	if !restored {
		guard = func(publish func() error) error { return a.publishWarm(ctx, s, publish) }
	}
	failures, err := s.syncResume(ctx, req.AdditionalDirectories, req.MCPServers, op.replay, op.owed, guard)
	if err != nil {
		return err
	}
	if r := op.replay; r != nil {
		op.owed = r.followup
		op.replay = nil
	}
	var result any = acp.ResumeSessionResponse{ConfigOptions: s.ConfigOptions(), AvailableCommands: availableCommands()}
	if restored {
		s.written = make(chan struct{})
		s.activation.Store(activationPending)
		if err = a.publishRestored(ctx, s); err != nil {
			return err
		}
		result = jsonrpc.OnWrite(result, s.beginWrite, s.endWrite)
	}
	if len(failures) == 0 {
		op.resolveSetup(result)
	} else {
		op.reply.Resolve(result)
		s.reportMCPSetup(a.ctx, failures, len(req.MCPServers), op.owed)
	}
	return nil
}

// publishWarm verifies durable ownership after preparation, while renewal and
// local reacquisition are fenced. The prepared change has no effect on loss.
func (a *Agent) publishWarm(ctx context.Context, s *Session, publish func() error) error {
	a.ownershipMu.Lock()
	defer a.ownershipMu.Unlock()
	err := a.cfg.Store.CheckOwner(ctx, s.id)
	switch {
	case errors.Is(err, store.ErrLost):
		// This operation returns the ownership error itself. Keep later
		// reservations intact so another resume can reacquire the session.
		a.dropLocked(s.id, nil)
		return errSessionLost()
	case err != nil:
		return jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot verify session ownership: %v", err)
	default:
		return publish()
	}
}

// publishRestored fences publication and the first save against lease renewal.
// MCP setup has already finished outside the ownership lock, so a slow server
// cannot prevent other sessions from renewing their leases.
func (a *Agent) publishRestored(ctx context.Context, s *Session) error {
	a.ownershipMu.Lock()
	defer a.ownershipMu.Unlock()
	if ctx.Err() != nil || a.ctx.Err() != nil {
		return acp.ErrRequestCancelled()
	}
	s.flush() // persist requested roots and any repaired history only after setup
	if s.isLost() {
		return errSessionLost()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Err() != nil || a.shuttingDown || a.ctx.Err() != nil {
		return acp.ErrRequestCancelled()
	}
	a.sessions[s.id] = s
	delete(a.lost, s.id)
	return nil
}

// open preserves a live local session only when Acquire proves this process
// still owned it. A restored session gets the request's directories, which
// Resume applies anyway: a saved directory removed since must not block the
// session. The bool identifies a private restored session whose caller must
// publish or close it.
//
// Only acquire runs under ownershipMu. The first catalog load can wait on the
// provider for catalogTimeout, which must not delay lease renewal; the session
// stays private until publishRestored, so nothing else can observe it meanwhile.
func (a *Agent) open(ctx context.Context, id, cwd string, dirs []string) (*Session, bool, error) {
	cached, l, err := a.acquire(ctx, id, cwd)
	if l == nil || err != nil {
		return cached, false, err
	}
	if cached != nil {
		// Join the retiring worker outside all locks, before synchronizing
		// replacement state. Its process-owned lease is now ours to keep.
		cached.Close()
	}
	c, err := a.catalog()
	if ctx.Err() != nil || a.ctx.Err() != nil {
		a.releaseID(id)
		return nil, false, acp.ErrRequestCancelled()
	}
	if err != nil {
		a.releaseID(id)
		return nil, false, turnError(err)
	}
	choice, err := c.initialSelection(modelSelection{l.Model, l.Effort}, a.cfg.DefaultModel)
	if err != nil {
		a.releaseID(id)
		return nil, false, turnError(err)
	}
	l.Model, l.Effort = choice.model, choice.effort
	l.Dirs = dirs
	s, err := restoreSession(a.ctx, a.deps, c, l)
	if err != nil {
		a.releaseID(id)
		return nil, false, acp.ErrInvalidParams("cannot restore the session: %v", err)
	}
	return s, true, nil
}

// acquire takes the lease and returns the cached session, if any. A non-nil
// record requires restoration and draining any cached session. Old saves must
// finish before ownership can return to this process, or their process-owner
// check could overwrite the new history. Mark the old session lost while its
// flushMu still fences queued flushes and insertions.
func (a *Agent) acquire(ctx context.Context, id, cwd string) (*Session, *store.Loaded, error) {
	a.ownershipMu.Lock()
	defer a.ownershipMu.Unlock()
	a.mu.Lock()
	old := a.sessions[id]
	a.mu.Unlock()
	if old != nil {
		// Reject a bad workspace before changing ownership of a cached
		// session. Its cwd is immutable for the session's lifetime.
		if old.cwd != cwd {
			return nil, nil, acp.ErrInvalidParams("cwd %q does not match the session's %q", cwd, old.cwd)
		}
		old.flushMu.Lock()
		defer old.flushMu.Unlock()
	}
	l, err := a.cfg.Store.Acquire(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, acp.ErrSessionNotFound(id)
	case errors.Is(err, store.ErrBusy):
		return nil, nil, errSessionBusy()
	case err != nil:
		return nil, nil, jsonrpc.NewError(jsonrpc.CodeInternalError, "cannot load the session: %v", err)
	}
	if l.Cwd != cwd {
		if old == nil {
			a.releaseID(id)
		}
		return nil, nil, acp.ErrInvalidParams("cwd %q does not match the session's %q", cwd, l.Cwd)
	}
	if old != nil {
		old.mu.Lock()
		if l.AlreadyOwned && !old.lost {
			old.mu.Unlock()
			return old, nil, nil
		}
		// Cancellation can remove the current cursor while Close drains.
		// Fence the old worker's output before detaching its generation.
		old.sealOutputLocked()
		old.loseLocked()
		old.mu.Unlock()
		a.mu.Lock()
		if a.sessions[id] == old {
			a.prompts.detach(id)
			a.detachSessionLocked(id)
		}
		a.lost[id] = true
		a.mu.Unlock()
	}
	return old, l, nil
}

// closeSession cancels the session's work at once, to keep ordering with
// later messages, and answers after the work has wound down and the session
// is released for other processes.
func (a *Agent) closeSession(ctx context.Context, req acp.CloseSessionRequest) (any, error) {
	a.mu.Lock()
	s := a.detachSessionLocked(req.SessionID)
	// Only pending setup can open an absent session. A close behind terminal
	// cleanup must wait for it without cancelling it (in particular, a delete
	// still needs its request context to remove the stored record).
	previous := a.operations[req.SessionID]
	for previous != nil && previous.prompt {
		previous = previous.previous
	}
	if previous != nil {
		select {
		case <-previous.done:
			previous = nil
		default:
		}
	}
	a.mu.Unlock()
	if s == nil && previous == nil {
		return acp.CloseSessionResponse{}, nil
	}
	interrupt := s != nil || (previous != nil && !previous.terminal)
	op := a.reserveOperation(ctx, req.SessionID, true, interrupt)
	op.closing = true
	if s != nil {
		s.sealOutput()
	}
	a.startOperation(req.SessionID, op, func(ctx context.Context) error {
		s = a.terminalSession(req.SessionID, s)
		if s != nil {
			s.Close()
			a.release(s)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		op.resolveSetup(acp.CloseSessionResponse{})
		return nil
	})
	return op.reply, nil
}

// deleteSession invalidates local access immediately, then waits for earlier
// setup cleanup before touching the stored record.
func (a *Agent) deleteSession(ctx context.Context, req acp.DeleteSessionRequest) (any, error) {
	a.mu.Lock()
	s := a.detachSessionLocked(req.SessionID)
	delete(a.lost, req.SessionID)
	a.mu.Unlock()
	op := a.reserveOperation(ctx, req.SessionID, true, true)
	if s != nil {
		s.sealOutput()
	}
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
		op.resolveSetup(acp.DeleteSessionResponse{})
		return nil
	})
	return op.reply, nil
}

// terminalSession catches a stored resume published between the handler's
// removal and cancellation of earlier setup. Later resumes wait for this op.
// Release/delete the lease only after all local resources have closed.
func (a *Agent) terminalSession(id string, captured *Session) *Session {
	a.mu.Lock()
	late := a.detachSessionLocked(id)
	a.mu.Unlock()
	if captured == nil {
		return late
	}
	if late != nil && late != captured {
		late.Close()
	}
	return captured
}

// detachSessionLocked removes the generation and revokes a queued activation
// before a later resume can publish another Session with the same wire ID.
func (a *Agent) detachSessionLocked(id string) *Session {
	s := a.sessions[id]
	if s != nil {
		s.detachActivation()
		s.activation.CompareAndSwap(activationActive, activationDetached)
		delete(a.sessions, id)
	}
	return s
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

func (a *Agent) setConfigOption(ctx context.Context, req acp.SetConfigOptionRequest) (any, error) {
	a.mu.Lock()
	s := a.sessions[req.SessionID]
	previous := a.operations[req.SessionID]
	state := activationDetached
	if s != nil {
		state = s.activation.Load()
	}
	lost := previous == nil && (state == activationLost || a.lost[req.SessionID])
	writing := state == activationWriting
	staged := writing || (previous != nil && previous.staged)
	barrier := previous
	if barrier != nil && barrier.config {
		barrier = barrier.modeBarrier
	}
	setupPending := state == activationActive && barrier.pending()
	a.mu.Unlock()
	reply := jsonrpc.NewReply(ctx)
	if state != activationActive && !writing {
		if lost {
			return a.rejectLater(reply, errSessionLost())
		}
		return a.rejectLater(reply, acp.ErrSessionNotFound(req.SessionID))
	}
	op := a.reserveOperation(ctx, req.SessionID, false, false)
	op.staged, op.config, op.modeBarrier = staged, true, barrier
	op.reply = reply
	op.owed = a.deps.replays.owe(req.SessionID)
	var selection *modelSelection
	if req.ConfigID == configModel || req.ConfigID == configEffort {
		selection = new(modelSelection)
		a.rememberSelection(op.done, selection)
	}
	a.startOperation(req.SessionID, op, func(ctx context.Context) error {
		if writing {
			if err := s.awaitWritten(ctx); err != nil {
				return err
			}
		}
		if err := a.checkGeneration(s); err != nil {
			return err
		}
		snapshot, err := s.setConfigOption(ctx, req.ConfigID, req.Type, req.Value, setupPending)
		if err != nil {
			return err
		}
		if selection != nil {
			*selection = snapshot.selection
		}
		s.publishConfigOption(reply, snapshot, op.owed)
		return nil
	})
	return reply, nil
}

// publishConfigOption completes committed work even if the request or session
// was cancelled in the meantime. The operation chain keeps later snapshots
// behind this one, and the replay obligation covers the wait for output capacity.
func (s *Session) publishConfigOption(reply *jsonrpc.Reply, snapshot configSnapshot, owed *outputObligation) {
	result := acp.SetConfigOptionResponse{ConfigOptions: snapshot.options}
	update := acp.ConfigOptionUpdate{ConfigOptions: snapshot.options}
	if owed.streams.wire == nil {
		if snapshot.changed {
			_ = s.notify(update)
		}
		reply.Resolve(result)
		return
	}
	var lines [][]byte
	if snapshot.changed {
		line, err := jsonrpc.NotificationLine(acp.MethodSessionUpdate, acp.SessionNotification{SessionID: s.id, Update: update})
		if err != nil {
			reply.Reject(err)
			return
		}
		lines = append(lines, line)
	}
	line, err := reply.ResultLine(result)
	if err != nil {
		reply.Reject(err)
		return
	}
	// Enqueue both lines at once so a cursor cannot split the pair.
	owed.sendReply(reply, append(lines, line)...)
}

// pendingSelection fences earlier client choices without waiting on the
// read loop. New sessions must see those choices, even before their saves finish.
func (a *Agent) pendingSelection() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selectionSaved
}

// modelSelection stays empty when a setter fails before mutation.
type modelSelection struct{ model, effort string }

// rememberSelection queues writes in request order across all sessions.
// Background session saves and resume never change this user preference.
func (a *Agent) rememberSelection(ready <-chan struct{}, choice *modelSelection) {
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
		if ready != nil {
			<-ready
		}
		if choice.model == "" {
			return
		}
		if err := a.cfg.Store.SetSelection(context.WithoutCancel(a.ctx), choice.model, choice.effort); err != nil {
			a.cfg.Log.Warn("saving last model selection", "err", err)
		}
	}()
}

func (a *Agent) session(id string) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil {
		switch s.activation.Load() {
		case activationActive:
			return s, nil
		case activationLost:
			return nil, errSessionLost()
		}
	}
	if a.lost[id] {
		return nil, errSessionLost()
	}
	return nil, acp.ErrSessionNotFound(id)
}

// checkGeneration revalidates queued work before handing it to the session.
func (a *Agent) checkGeneration(s *Session) error {
	current, err := a.session(s.id)
	if err == nil && current != s {
		return errSessionLost()
	}
	return err
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
