// Package agent implements the ACP agent: session methods, the prompt
// lifecycle with its queue and cancellation, and the model/tool loop.
package agent

import (
	"cmp"
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
	"github.com/anesthetised/acplause/internal/tools"
)

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
	Info      acp.Implementation
	MaxRounds int // model requests per turn; default 25
	Tools     tools.Options
	Log       *slog.Logger
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

	mu       sync.Mutex
	sessions map[string]*Session
}

// New creates an agent and starts loading the model catalog.
func New(cfg Config, out Client, model Model, listModels ListModels) *Agent {
	cfg.MaxRounds = cmp.Or(cfg.MaxRounds, 25)
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		cfg:      cfg,
		deps:     deps{out: out, model: model, maxRounds: cfg.MaxRounds, tools: cfg.Tools, log: cfg.Log},
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[string]*Session),
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
	return a
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
		acp.MethodSessionSetConfig: a.ready(jsonrpc.Handle(a.setConfigOption)),
	}
}

// Shutdown cancels all work and closes every session.
func (a *Agent) Shutdown() {
	a.mu.Lock()
	sessions := slices.Collect(maps.Values(a.sessions))
	clear(a.sessions)
	a.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(s.Close)
	}
	wg.Wait()
	a.cancel()
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
				Prompt:                &acp.PromptCapabilities{},
				AdditionalDirectories: &acp.Supported{},
			},
		},
	}, nil
}

// newSession answers once the model catalog is loaded, which only delays
// the first session of a connection.
func (a *Agent) newSession(_ context.Context, req acp.NewSessionRequest) (any, error) {
	reply := jsonrpc.NewReply()
	go func() {
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
		a.mu.Lock()
		a.sessions[s.id] = s
		a.mu.Unlock()
		reply.Resolve(acp.NewSessionResponse{SessionID: s.id, ConfigOptions: s.ConfigOptions()})
	}()
	return reply, nil
}

func (a *Agent) prompt(ctx context.Context, req acp.PromptRequest) (any, error) {
	s, err := a.session(req.SessionID)
	if err != nil {
		return nil, err
	}
	return s.Submit(ctx, req.Prompt), nil
}

func (a *Agent) cancelSession(_ context.Context, n acp.CancelSessionNotification) (any, error) {
	s, err := a.session(n.SessionID)
	if err != nil {
		return nil, err
	}
	s.Cancel()
	return nil, nil
}

func (a *Agent) listSessions(_ context.Context, req acp.ListSessionsRequest) (any, error) {
	offset, err := decodeCursor(req.Cursor)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	var infos []acp.SessionInfo
	for _, s := range a.sessions {
		if info := s.Info(); req.Cwd == "" || info.Cwd == req.Cwd {
			infos = append(infos, info)
		}
	}
	a.mu.Unlock()

	// Most recently active first; RFC 3339 UTC timestamps sort as strings.
	slices.SortFunc(infos, func(x, y acp.SessionInfo) int {
		return cmp.Or(cmp.Compare(y.UpdatedAt, x.UpdatedAt), cmp.Compare(x.SessionID, y.SessionID))
	})
	resp := acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}
	if offset < len(infos) {
		end := min(offset+listPageSize, len(infos))
		resp.Sessions = infos[offset:end]
		if end < len(infos) {
			resp.NextCursor = encodeCursor(end)
		}
	}
	return resp, nil
}

func (a *Agent) resumeSession(_ context.Context, req acp.ResumeSessionRequest) (any, error) {
	s, err := a.session(req.SessionID)
	if err != nil {
		return nil, err
	}
	if req.Cwd != s.cwd {
		return nil, acp.ErrInvalidParams("cwd %q does not match the session's %q", req.Cwd, s.cwd)
	}
	replay := req.ReplayFrom != nil
	if replay && req.ReplayFrom.Type != acp.ReplayFromStart {
		return nil, acp.ErrInvalidParams("unsupported replayFrom type %q", req.ReplayFrom.Type)
	}
	if err := s.Resume(req.AdditionalDirectories, replay); err != nil {
		return nil, acp.ErrInvalidParams("cannot open workspace: %v", err)
	}
	return acp.ResumeSessionResponse{ConfigOptions: s.ConfigOptions()}, nil
}

// closeSession cancels the session's work at once, to keep ordering with
// later messages, and answers after the work has wound down.
func (a *Agent) closeSession(_ context.Context, req acp.CloseSessionRequest) (any, error) {
	a.mu.Lock()
	s, ok := a.sessions[req.SessionID]
	delete(a.sessions, req.SessionID)
	a.mu.Unlock()
	if !ok {
		return nil, acp.ErrSessionNotFound(req.SessionID)
	}

	s.Cancel()
	reply := jsonrpc.NewReply()
	go func() {
		s.Close()
		reply.Resolve(acp.CloseSessionResponse{})
	}()
	return reply, nil
}

func (a *Agent) setConfigOption(_ context.Context, req acp.SetConfigOptionRequest) (any, error) {
	s, err := a.session(req.SessionID)
	if err != nil {
		return nil, err
	}
	if err := s.SetConfigOption(req.ConfigID, req.Type, req.Value); err != nil {
		return nil, err
	}
	return acp.SetConfigOptionResponse{ConfigOptions: s.ConfigOptions()}, nil
}

func (a *Agent) session(id string) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sessions[id]; ok {
		return s, nil
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
