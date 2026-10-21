package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/mcp"
	"github.com/anesthetised/acplause/internal/tools"
)

// Execution modes are session state, never a remembered user preference.
const (
	modeBuild = "build"
	modePlan  = "plan"
)

// deps is what sessions share with the agent.
type deps struct {
	forms           *atomic.Bool // negotiated capabilities belong to this connection, never the store
	out             Client
	model           Model
	readConcurrency int
	maxRounds       int
	mcpTimeout      time.Duration
	turnTimeout     time.Duration
	maxTurnTokens   uint64
	maxTurnCostUSD  float64
	tools           tools.Options
	log             *slog.Logger
	store           Store // nil keeps sessions in memory only
}

// Session is one conversation. Prompts queue up and run one turn at a time
// on a worker goroutine that exists only while there is work.
type Session struct {
	id  string
	cwd string
	// instructions is the last worker-refreshed root AGENTS.md, guarded by mu.
	instructions string
	deps         deps
	catalog      *catalog

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	budget *turnBudget // owned only by the worker during one accepted turn; never persisted

	mu            sync.Mutex
	dirs          []string
	mcp           *mcp.Manager
	toolbox       *tools.Toolbox
	activeToolbox *tools.Toolbox // retained by the single worker during a tool or instruction read
	model         string
	mode          string
	effort        string
	title         string
	updatedAt     time.Time
	cost          float64         // cumulative USD
	history       []llm.Message   // what the model sees
	transcript    []acp.Update    // what the client saw, for replay
	planID        string          // current accepted turn's plan; empty until its first update
	planIndex     map[string]int  // plan IDs to their first transcript positions; rebuilt on restore
	state         acp.StateUpdate // last reported foreground state
	queue         []*submission   // prompts not yet inserted
	worker        chan struct{}   // closed when the running worker exits; nil when idle
	stopTurn      context.CancelFunc
	closed        bool

	// Persistence (persist.go). flushMu serialises saves and is taken
	// before mu.
	flushMu     sync.Mutex
	saves       sync.WaitGroup // admitted under mu; Close waits before releasing ownership
	saved       int            // history messages after the system prompt already stored
	dirty       map[int]bool   // transcript entries changed since the last save
	lost        bool           // another process took the session over
	saveWarning bool           // suppress repeated notices until a save succeeds
}

// submission is a prompt waiting for insertion.
type submission struct {
	ctx       context.Context // the session/prompt request's context
	prompt    []acp.ContentBlock
	reply     *jsonrpc.Reply
	stopWatch func() bool // stops watching ctx for cancellation
}

func newSession(parent context.Context, d deps, c *catalog, cwd string, dirs []string) (*Session, error) {
	toolbox, err := tools.New(cwd, dirs, d.tools)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	return &Session{
		id:        newID(),
		cwd:       cwd,
		deps:      d,
		catalog:   c,
		ctx:       ctx,
		cancel:    cancel,
		dirs:      dirs,
		toolbox:   toolbox,
		mcp:       mcp.New(ctx, cwd, d.log),
		mode:      modeBuild,
		effort:    effortDefault,
		updatedAt: time.Now(),
		history:   []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt(cwd, dirs, "", modeBuild)}},
		state:     acp.StateUpdate{State: acp.StateIdle},
		dirty:     map[int]bool{},
		planIndex: map[string]int{},
	}, nil
}

// Submit queues a prompt. The reply resolves with the user message's id
// once it is inserted, or fails with Request cancelled if the prompt is
// cancelled first, through ctx or session/cancel.
func (s *Session) Submit(ctx context.Context, prompt []acp.ContentBlock) *jsonrpc.Reply {
	reply := jsonrpc.NewReply(ctx)
	s.submit(ctx, prompt, reply)
	return reply
}

func (s *Session) submit(ctx context.Context, prompt []acp.ContentBlock, reply *jsonrpc.Reply) {
	sub := &submission{ctx: ctx, prompt: prompt, reply: reply}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil {
		sub.reply.Reject(acp.ErrRequestCancelled())
		return
	}
	if s.lost {
		sub.reply.Reject(errSessionLost())
		return
	}
	if s.model == "" {
		sub.reply.Reject(acp.ErrInvalidParams("select a model before submitting a prompt"))
		return
	}
	s.queue = append(s.queue, sub)
	sub.stopWatch = context.AfterFunc(ctx, func() { s.withdraw(sub) })
	if s.worker == nil {
		s.worker = make(chan struct{})
		go s.work(s.worker)
	}
}

// Cancel stops the running turn and drops queued prompts.
func (s *Session) Cancel() {
	s.mu.Lock()
	queued := s.queue
	s.queue = nil
	if s.stopTurn != nil {
		s.stopTurn()
	}
	s.mu.Unlock()
	for _, sub := range queued {
		sub.stopWatch()
		sub.reply.Reject(acp.ErrRequestCancelled())
	}
}

// Close cancels all work, waits for the final updates of a running turn and
// releases the session's resources.
func (s *Session) Close() {
	s.mu.Lock()
	s.closed = true
	worker := s.worker
	s.mu.Unlock()

	s.Cancel()
	s.mcp.Close()
	if worker != nil {
		<-worker
	}
	s.cancel()
	s.saves.Wait()
	s.closeToolbox(s.toolbox)
}

// Resume reattaches the client: it replaces the additional directories,
// optionally replays the history, and reports the current state.
func (s *Session) Resume(ctx context.Context, dirs []string, servers []acp.MCPServer, replay bool) error {
	s.mu.Lock()
	changed := !slices.Equal(dirs, s.dirs)
	s.mu.Unlock()
	var toolbox *tools.Toolbox
	var err error
	if changed {
		toolbox, err = tools.New(s.cwd, dirs, s.deps.tools)
		if err != nil {
			return err
		}
	}
	change, err := s.mcp.Prepare(ctx, mcpConfigs(servers))
	if err != nil {
		if toolbox != nil {
			s.closeToolbox(toolbox)
		}
		return err
	}
	defer change.Discard()
	s.mu.Lock()
	if s.closed || s.lost || ctx.Err() != nil {
		s.mu.Unlock()
		if toolbox != nil {
			s.closeToolbox(toolbox)
		}
		return errors.New("session setup was cancelled")
	}
	if err = change.Commit(); err != nil {
		s.mu.Unlock()
		if toolbox != nil {
			s.closeToolbox(toolbox)
		}
		return err
	}
	var old *tools.Toolbox
	if changed {
		old = s.toolbox
		s.toolbox, s.dirs = toolbox, dirs
		s.history[0].Content = systemPrompt(s.cwd, dirs, s.instructions, s.mode)
		if old == s.activeToolbox {
			old = nil
		}
		s.flushAsync()
	}
	if replay {
		for _, u := range s.transcript {
			s.send(u)
		}
	}
	s.send(s.state)
	s.mu.Unlock()
	if old != nil {
		s.closeToolbox(old)
	}
	return nil
}

// acquireToolbox pins roots for one worker operation, including a read group,
// not an entire model request. Resume can change roots while the model streams.
func (s *Session) acquireToolbox() *tools.Toolbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeToolbox = s.toolbox
	return s.toolbox
}

func (s *Session) releaseToolbox(tb *tools.Toolbox) {
	s.mu.Lock()
	s.activeToolbox = nil
	retired := tb != s.toolbox
	s.mu.Unlock()
	if retired {
		s.closeToolbox(tb)
	}
}

func (s *Session) closeToolbox(tb *tools.Toolbox) {
	if err := tb.Close(); err != nil {
		s.deps.log.Warn("closing workspace", "session", s.id, "err", err)
	}
}

// ConfigOptions lists the session's settings.
func (s *Session) ConfigOptions() []acp.ConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalog.options(s.model, s.effort, s.mode)
}

// setConfigOption admits a mode change under the same locks as setup and prompts.
func (s *Session) setConfigOption(id, valueType string, raw json.RawMessage, setupPending bool) error {
	var value string
	if valueType != acp.ConfigValueID || json.Unmarshal(raw, &value) != nil {
		return acp.ErrInvalidParams("%s takes an id value", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch id {
	case configMode:
		if value != modeBuild && value != modePlan {
			return acp.ErrInvalidParams("unknown execution mode %q", value)
		}
		if s.mode == value {
			return nil
		}
		if setupPending || s.worker != nil || len(s.queue) > 0 || s.state.State != acp.StateIdle {
			return acp.ErrInvalidParams("cannot change mode while the session is busy; cancel or wait for work and setup to finish, then retry")
		}
		if s.closed || s.lost {
			return acp.ErrInvalidParams("cannot change mode on a closed or lost session")
		}
		s.mode = value
		s.history[0].Content = systemPrompt(s.cwd, s.dirs, s.instructions, s.mode)
	case configModel:
		if !s.catalog.has(value) {
			return acp.ErrInvalidParams("unknown model %q", value)
		}
		s.model = value
	case configEffort:
		if !s.catalog.reasons(s.model) {
			return acp.ErrInvalidParams("model %s does not take a reasoning effort", s.model)
		}
		if !validEffort(value) {
			return acp.ErrInvalidParams("unknown reasoning effort %q", value)
		}
		s.effort = value
	default:
		return acp.ErrInvalidParams("unknown config option %q", id)
	}
	s.flushAsync()
	return nil
}

// withdraw drops a prompt whose request was cancelled before insertion.
func (s *Session) withdraw(sub *submission) {
	s.mu.Lock()
	i := slices.Index(s.queue, sub)
	if i >= 0 {
		s.queue = slices.Delete(s.queue, i, i+1)
	}
	s.mu.Unlock()
	if i >= 0 {
		sub.reply.Reject(acp.ErrRequestCancelled())
	}
}

func (s *Session) work(done chan struct{}) {
	defer close(done)
	for {
		sub, ctx := s.next()
		if sub == nil {
			return
		}
		s.runTurn(ctx, sub)
	}
}

// next takes the next queued prompt and a context for its turn, or retires
// the worker when the queue is empty.
func (s *Session) next() (*submission, context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopTurn != nil {
		s.stopTurn()
		s.stopTurn = nil
	}
	if len(s.queue) == 0 {
		s.worker = nil
		return nil, nil
	}
	sub := s.queue[0]
	s.queue = s.queue[1:]
	sub.stopWatch()
	ctx, cancel := context.WithCancel(s.ctx)
	s.stopTurn = cancel
	return sub, ctx
}

var errTurnTimeout = errors.New("turn time limit reached")

// runTurn inserts the prompt before starting its optional work deadline. The
// outer context retains user/session cancellation independently of that timer.
func (s *Session) runTurn(ctx context.Context, sub *submission) {
	if !s.insert(ctx, sub) {
		return
	}
	outer := ctx
	s.budget = &turnBudget{maxTokens: s.deps.maxTurnTokens, maxCost: s.deps.maxTurnCostUSD}
	defer func() { s.budget = nil }()
	if s.deps.turnTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, s.deps.turnTimeout, errTurnTimeout)
		defer cancel()
	}
	var stop acp.StopReason
	var err *jsonrpc.Error
	if isCompactCommand(sub.prompt) {
		stop, err = s.compactCommand(ctx)
	} else {
		stop, err = s.converse(ctx)
	}
	// Serialize finalization with Cancel, close/loss and replay. An outer
	// cancellation that raced the deadline must not produce a timeout notice.
	s.mu.Lock()
	if ctx.Err() != nil || outer.Err() != nil {
		stop, err = acp.StopCancelled, nil
		if context.Cause(ctx) == errTurnTimeout && outer.Err() == nil && !s.closed && !s.lost {
			s.send(acp.Notice{
				Severity: "warning", Title: "Turn time limit reached",
				Description: fmt.Sprintf("The configured turn time limit of %s elapsed.", s.deps.turnTimeout),
			})
		}
	}
	s.state = acp.StateUpdate{State: acp.StateIdle, StopReason: stop, Error: err}
	s.send(s.state)
	s.mu.Unlock()
	if err != nil {
		s.deps.log.Warn("turn failed", "session", s.id, "err", err)
	}
	s.flush()
}

// insert adds the user message to the conversation and acknowledges the
// prompt, unless it was cancelled first. For an individual request the response
// precedes user_message on the wire; a batch collects replies before writing
// its response array, so updates may precede that array.
func (s *Session) insert(ctx context.Context, sub *submission) bool {
	// Validation runs on the session worker, never the protocol read loop.
	// Keep both cancellation sources live while decoding and claiming the turn.
	saveCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(sub.ctx, cancel)
	defer stop()
	defer cancel()
	images, imageErr := promptImages(saveCtx, sub.prompt)
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Loss cancels the turn too; report the cause, not the cancellation.
	if s.lost {
		sub.reply.Reject(errSessionLost())
		return false
	}
	if ctx.Err() != nil || sub.ctx.Err() != nil {
		sub.reply.Reject(acp.ErrRequestCancelled())
		return false
	}
	if imageErr != nil {
		sub.reply.Reject(acp.ErrInvalidParams("invalid prompt images: %v", imageErr))
		return false
	}
	if len(images) > 0 {
		if s.model == "" {
			sub.reply.Reject(acp.ErrInvalidParams("select a model before submitting a prompt"))
			return false
		}
		if !s.catalog.vision(s.model) {
			sub.reply.Reject(acp.ErrInvalidParams("selected model %q is not known to accept images", s.model))
			return false
		}
	}
	msg := acp.UserMessage{MessageID: newID(), Content: sub.prompt}
	user := llm.Message{Role: llm.RoleUser, Content: promptText(sub.prompt), Images: images}
	updated := time.Now()
	// Until the transaction commits, neither replay nor the model may see
	// the prompt. Cancellation remains available while SQLite is busy.
	if err := s.claimTurn(saveCtx, msg, user, updated); err != nil {
		sub.reply.Reject(err)
		return false
	}
	s.history = append(s.history, user)
	s.transcript = append(s.transcript, msg)
	s.planID = "" // A rejected or merely queued prompt must not reset the plan.
	s.dirty[len(s.transcript)-1] = true
	first := s.title == ""
	if first {
		s.title = title(sub.prompt)
	}
	s.updatedAt = updated
	sub.reply.Resolve(acp.PromptResponse{MessageID: msg.MessageID})
	s.send(msg)
	if first {
		// Not in the transcript: replay is conversation history, and
		// session/list already reports the title.
		s.send(acp.SessionInfoUpdate{Title: s.title, UpdatedAt: s.updatedAt.UTC().Format(time.RFC3339Nano)})
	}
	s.state = acp.StateUpdate{State: acp.StateRunning}
	s.send(s.state)
	return true
}

// setState reports a foreground state change. It holds the lock so a resume
// snapshot can never be sent out of order with live changes.
func (s *Session) setState(u acp.StateUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = u
	s.send(u)
}

// send requires s.mu so replay snapshots and live updates cannot interleave.
func (s *Session) send(u acp.Update) {
	n := acp.SessionNotification{SessionID: s.id, Update: u}
	if err := s.deps.out.Notify(acp.MethodSessionUpdate, n); err != nil {
		s.deps.log.Warn("dropping session update", "session", s.id, "err", err)
	}
}

func (s *Session) appendHistory(m llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, m)
	s.updatedAt = time.Now()
}

// newID returns a random UUIDv4.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
