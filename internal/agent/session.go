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
	replays         *replayStreams
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
	// calibration is shared by every session of the process; nil keeps the
	// uncalibrated byte estimate.
	calibration *calibration
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

	written       chan struct{} // closed when this cold response finishes writing
	activation    atomic.Uint32 // cold-response delivery may race generation detachment
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
	claimed       bool            // a prompt left the queue and its turn has not ended
	stopTurn      context.CancelCauseFunc
	rejections    sync.WaitGroup // queued replies detached under mu, settled before Close returns
	closeOnce     sync.Once      // renewal, resume and close can join the same retiring generation
	closed        bool

	terminalOutput *outputObligation // owned by the retiring generation until its last update

	// Private lookup and persistence indexes for inserted user messages, guarded by mu.
	keys           map[string]string // key to client-visible message ID
	transcriptKeys map[int]string    // transcript position to key

	// shellDecisions holds ADR 0029's remembered bash choices, guarded by mu;
	// never persisted, cleared when the roots change.
	shellDecisions map[string]bool
	// rootsGen counts root changes, so an "always" answer to a request sent
	// under older roots is not recorded under the new ones.
	rootsGen uint64
	// Persistence (persist.go). flushMu serialises saves and is taken
	// before mu.
	flushMu     sync.Mutex
	saves       sync.WaitGroup // admitted under mu; Close waits before releasing ownership
	saved       int            // history messages after the system prompt already stored
	dirty       map[int]bool   // transcript entries changed since the last save
	lost        bool           // another process took the session over
	saveWarning bool           // suppress repeated notices until a save succeeds
}

const (
	activationActive uint32 = iota // new sessions and continuously owned live sessions
	activationPending
	activationWriting
	activationDetached
	activationLost
)

// beginWrite permits staging against this generation while its response is in Write.
func (s *Session) beginWrite() { s.activation.CompareAndSwap(activationPending, activationWriting) }

func (s *Session) endWrite(err error) {
	next := activationActive
	if err != nil {
		next = activationDetached
	}
	s.activation.CompareAndSwap(activationWriting, next)
	close(s.written)
}

func (s *Session) awaitWritten(ctx context.Context) error {
	select {
	case <-s.written:
		if ctx.Err() == nil && s.activation.Load() == activationActive {
			return nil
		}
	case <-s.ctx.Done():
	case <-ctx.Done():
	}
	return acp.ErrRequestCancelled()
}

// detachActivation fences callbacks both before and during the response write.
func (s *Session) detachActivation() {
	for {
		state := s.activation.Load()
		if state != activationPending && state != activationWriting {
			return
		}
		if s.activation.CompareAndSwap(state, activationDetached) {
			return
		}
	}
}

// submission is a prompt waiting for insertion.
type submission struct {
	ctx       context.Context // the session/prompt request's context
	prompt    []acp.ContentBlock
	key       string
	compact   bool
	reply     requestReply
	stopWatch func() bool // stops watching ctx for cancellation
}

func newSession(parent context.Context, d deps, c *catalog, cwd string, dirs []string) (*Session, error) {
	toolbox, err := tools.New(cwd, dirs, d.tools)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	if d.replays == nil {
		d.replays = newReplayStreams(d.out)
	}
	s := &Session{
		id:             newID(),
		cwd:            cwd,
		deps:           d,
		catalog:        c,
		ctx:            ctx,
		cancel:         cancel,
		dirs:           dirs,
		toolbox:        toolbox,
		mcp:            mcp.New(ctx, cwd, d.log),
		mode:           modeBuild,
		effort:         effortDefault,
		updatedAt:      time.Now(),
		history:        []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt(cwd, dirs, "", modeBuild)}},
		state:          acp.StateUpdate{State: acp.StateIdle},
		dirty:          map[int]bool{},
		keys:           make(map[string]string),
		transcriptKeys: make(map[int]string),
		planIndex:      map[string]int{},
	}
	s.deps.out = replayClient{d.out, s}
	return s, nil
}

// Submit queues a prompt. The reply resolves with the user message's id
// once it is inserted, or fails with Request cancelled if the prompt is
// cancelled first, through ctx or session/cancel.
func (s *Session) Submit(ctx context.Context, prompt []acp.ContentBlock) *jsonrpc.Reply {
	reply := jsonrpc.NewReply(ctx)
	s.submit(&submission{ctx: ctx, prompt: prompt, reply: reply}, nil)
	return reply
}

func (sub *submission) cancelled() bool {
	if sub.ctx.Err() != nil {
		return true
	}
	g, ok := sub.reply.(*promptGroup)
	return ok && g.cancelled()
}

func (s *Session) submit(sub *submission, setupErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := sub.reply.(*promptGroup); ok {
		g.setStaged(false)
	}
	if s.closed {
		sub.reply.Reject(acp.ErrRequestCancelled())
		return
	}
	if s.lost {
		sub.reply.Reject(errSessionLost())
		return
	}
	// A recorded key resolves the original prompt before command validation.
	if id, ok := s.keys[sub.key]; ok {
		if s.deps.store == nil {
			sub.reply.Resolve(acp.PromptResponse{MessageID: id})
		} else {
			s.saves.Go(func() { s.resolveRecorded(sub.reply, id) })
		}
		return
	}
	if setupErr != nil {
		sub.reply.Reject(setupErr)
		return
	}
	if sub.cancelled() {
		sub.reply.Reject(acp.ErrRequestCancelled())
		return
	}
	compact, err := classifyCompact(sub.prompt)
	if err != nil {
		s.rejectSubmissionsLocked(err, sub)
		return
	}
	if s.model == "" {
		sub.reply.Reject(acp.ErrInvalidParams("select a model before submitting a prompt"))
		return
	}
	sub.compact = compact
	s.queue = append(s.queue, sub)
	sub.stopWatch = context.AfterFunc(sub.ctx, func() { s.withdraw(sub) })
	if s.worker == nil {
		s.worker = make(chan struct{})
		go s.work(s.worker)
	}
}

var errSessionCancel = errors.New("explicit session cancellation")

// Cancel handles session/cancel for the running turn and queued prompts.
func (s *Session) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelLocked(errSessionCancel)
}

func (s *Session) cancelLocked(cause error) {
	if s.stopTurn != nil {
		s.stopTurn(cause)
	}
	s.rejectSubmissionsLocked(acp.ErrRequestCancelled(), s.queue...)
	s.queue = nil
}

// rejectSubmissionsLocked ends admission to each rejected group on the reader.
// Only writing the replies can wait for stdout. Close joins that work.
func (s *Session) rejectSubmissionsLocked(err error, submissions ...*submission) {
	if len(submissions) == 0 {
		return
	}
	replies := make([]requestReply, 0, len(submissions))
	for _, sub := range submissions {
		if sub.stopWatch != nil {
			sub.stopWatch()
		}
		if group, ok := sub.reply.(*promptGroup); ok {
			for _, w := range group.takeWaiters() {
				replies = append(replies, w.reply)
			}
		} else {
			replies = append(replies, sub.reply)
		}
	}
	s.rejections.Go(func() {
		for _, reply := range replies {
			reply.Reject(err)
		}
	})
}

// sealOutput withdraws the queue on the reader before the next cursor arrives.
func (s *Session) sealOutput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealOutputLocked()
}

// A close and a discontinuous acquire can retire the same generation. Only
// a claimed turn can still publish updates once the queue is withdrawn.
func (s *Session) sealOutputLocked() {
	if s.claimed && s.terminalOutput == nil {
		s.terminalOutput = s.deps.replays.owe(s.id)
	}
	s.closed = true
	s.cancelLocked(nil)
}

// Close cancels all work, waits for the final updates of a running turn and
// releases the session's resources.
func (s *Session) Close() {
	s.closeOnce.Do(s.close)
}

func (s *Session) close() {
	s.detachActivation()
	s.mu.Lock()
	s.closed = true
	worker := s.worker
	s.cancelLocked(nil)
	s.mu.Unlock()

	s.mcp.Close()
	if worker != nil {
		<-worker
	}
	s.cancel()
	s.saves.Wait()
	s.rejections.Wait()
	s.closeToolbox(s.toolbox)
}

// Resume reattaches the client: it replaces the additional directories,
// optionally replays the history, and reports current metadata and state.
func (s *Session) Resume(ctx context.Context, dirs []string, servers []acp.MCPServer, replay bool) error {
	var r *historyReplay
	if replay {
		var err error
		r, err = s.deps.replays.begin(ctx, s.id, nil)
		if err != nil {
			return err
		}
		defer r.cancel()
		ctx = r.ctx
	}
	err := s.syncResume(ctx, dirs, servers, r, nil)
	if r != nil {
		return r.finish(ctx, err)
	}
	return err
}

// syncResume captures metadata and state in the same stream as live changes,
// so newer values cannot be overwritten while history is being delivered.
func (s *Session) syncResume(ctx context.Context, dirs []string, servers []acp.MCPServer, r *historyReplay, owed *outputObligation) error {
	if r != nil && r.session != s {
		r.snapshot(s)
	}
	if err := s.configureResume(ctx, dirs, servers); err != nil {
		return acp.ErrInvalidParams("cannot configure session: %v", err)
	}
	if r == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.live(ctx, sessionInfoPatch(s.title, s.updatedAt), nil); err != nil {
			return err
		}
		return s.live(ctx, s.state, owed)
	}
	if err := r.stageCurrent(s); err != nil {
		return err
	}
	return r.write(ctx)
}

// configureResume replaces the workspace and MCP configuration transactionally.
func (s *Session) configureResume(ctx context.Context, dirs []string, servers []acp.MCPServer) error {
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
		clear(s.shellDecisions)
		s.rootsGen++
		s.history[0].Content = systemPrompt(s.cwd, dirs, s.instructions, s.mode)
		if old == s.activeToolbox {
			old = nil
		}
		s.flushAsync()
	}
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

type configSnapshot struct {
	options   []acp.ConfigOption
	selection modelSelection
	changed   bool
}

// setConfigOption captures the full advertised snapshot at the mutation.
// The caller owns the operation reservation and publishes outside mu.
func (s *Session) setConfigOption(ctx context.Context, id, valueType string, raw json.RawMessage, setupPending bool) (configSnapshot, error) {
	var value string
	if valueType != acp.ConfigValueID || json.Unmarshal(raw, &value) != nil {
		return configSnapshot{}, acp.ErrInvalidParams("%s takes an id value", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.closed || s.lost || s.activation.Load() != activationActive {
		return configSnapshot{}, acp.ErrRequestCancelled()
	}
	old := modelSelection{s.model, s.effort}
	oldMode := s.mode
	switch id {
	case configMode:
		if value != modeBuild && value != modePlan {
			return configSnapshot{}, acp.ErrInvalidParams("unknown execution mode %q", value)
		}
		if s.mode != value {
			// The worker outlives the turn while it saves; only a claimed turn is busy.
			if setupPending || s.claimed || len(s.queue) > 0 || s.state.State != acp.StateIdle {
				return configSnapshot{}, acp.ErrInvalidParams("cannot change mode while the session is busy; cancel or wait for work and setup to finish, then retry")
			}
			s.mode = value
			s.history[0].Content = systemPrompt(s.cwd, s.dirs, s.instructions, s.mode)
		}
	case configModel:
		if !s.catalog.has(value) {
			return configSnapshot{}, acp.ErrInvalidParams("unknown model %q", value)
		}
		s.model = value
	case configEffort:
		if !s.catalog.reasons(s.model) {
			return configSnapshot{}, acp.ErrInvalidParams("model %s does not take a reasoning effort", s.model)
		}
		if !validEffort(value) {
			return configSnapshot{}, acp.ErrInvalidParams("unknown reasoning effort %q", value)
		}
		s.effort = value
	default:
		return configSnapshot{}, acp.ErrInvalidParams("unknown config option %q", id)
	}
	selection := modelSelection{s.model, s.effort}
	changed := selection != old || s.mode != oldMode
	if changed || id != configMode {
		s.flushAsync()
	}
	return configSnapshot{s.catalog.options(s.model, s.effort, s.mode), selection, changed}, nil
}

// withdraw drops a prompt whose request was cancelled before insertion.
func (s *Session) withdraw(sub *submission) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.queue, sub); i >= 0 {
		s.rejectSubmissionsLocked(acp.ErrRequestCancelled(), sub)
		s.queue = slices.Delete(s.queue, i, i+1)
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
		s.stopTurn(nil)
		s.stopTurn = nil
	}
	s.claimed = len(s.queue) > 0
	if len(s.queue) == 0 {
		s.terminalOutput.release() // a withdrawn claim can finish without an idle update
		s.worker = nil
		return nil, nil
	}
	sub := s.queue[0]
	s.queue = s.queue[1:]
	sub.stopWatch()
	ctx, cancel := context.WithCancelCause(s.ctx)
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
	if sub.compact {
		stop, err = s.compactCommand(ctx)
	} else {
		stop, err = s.converse(ctx)
	}
	// Serialize finalization with Cancel, close/loss and replay. An outer
	// cancellation that raced the deadline must not produce a timeout notice.
	s.mu.Lock()
	if ctx.Err() != nil || outer.Err() != nil {
		stop, err = acp.StopCancelled, nil
		if context.Cause(ctx) == errSessionCancel && !s.lost {
			s.removeUnfinishedPlan()
		}
		if context.Cause(ctx) == errTurnTimeout && outer.Err() == nil && !s.closed && !s.lost {
			s.send(acp.Notice{
				Severity: "warning", Title: "Turn time limit reached",
				Description: fmt.Sprintf("The configured turn time limit of %s elapsed.", s.deps.turnTimeout),
			})
		}
	}
	s.state = acp.StateUpdate{State: acp.StateIdle, StopReason: stop, Error: err}
	s.claimed = false
	if err := s.live(context.Background(), s.state, s.terminalOutput); err != nil {
		s.deps.log.Warn("dropping final state", "session", s.id, "err", err)
	}
	s.mu.Unlock()
	if err != nil {
		s.deps.log.Warn("turn failed", "session", s.id, "err", err)
	}
	s.flush()
}

// insert adds the user message to the conversation and acknowledges the
// prompt, unless it was cancelled first. The acknowledgement is queued
// before user_message so the client can match the echo to the accepted prompt.
func (s *Session) insert(ctx context.Context, sub *submission) bool {
	// Validation runs on the session worker, never the protocol read loop.
	// Keep both cancellation sources live while decoding and claiming the turn.
	saveCtx, cancel := context.WithCancel(ctx)
	if group, ok := sub.reply.(*promptGroup); ok {
		group.bindSave(cancel)
	}
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
	if ctx.Err() != nil || sub.cancelled() {
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
	if sub.compact {
		user.LocalCommand = compactName
	}
	updated := time.Now()
	// Until the transaction commits, neither replay nor the model may see
	// the prompt. Cancellation remains available while SQLite is busy.
	if err := s.claimTurn(saveCtx, sub, msg, user, updated); err != nil {
		sub.reply.Reject(err)
		return false
	}
	s.history = append(s.history, user)
	if sub.key != "" {
		s.keys[sub.key] = msg.MessageID
		s.transcriptKeys[len(s.transcript)] = sub.key
	}
	s.transcript = append(s.transcript, msg)
	s.planID = "" // A rejected or merely queued prompt must not reset the plan.
	s.dirty[len(s.transcript)-1] = true
	var newTitle string
	if s.title == "" {
		newTitle = title(sub.prompt)
		s.title = newTitle
	}
	s.updatedAt = updated
	sub.reply.Resolve(acp.PromptResponse{MessageID: msg.MessageID})
	s.send(msg)
	s.send(sessionInfoPatch(newTitle, s.updatedAt))
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
// During a replay it queues u behind the snapshot instead.
func (s *Session) send(u acp.Update) {
	if err := s.live(context.Background(), u, nil); err != nil {
		s.deps.log.Warn("dropping session update", "session", s.id, "err", err)
	}
}

func (s *Session) notify(u acp.Update) error {
	n := acp.SessionNotification{SessionID: s.id, Update: u}
	err := s.deps.out.Notify(acp.MethodSessionUpdate, n)
	if err != nil {
		s.deps.log.Warn("dropping session update", "session", s.id, "err", err)
	}
	return err
}

func (s *Session) appendHistory(m llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, m)
	s.updatedAt = time.Now()
	s.send(sessionInfoPatch("", s.updatedAt))
}

// sessionInfoPatch keeps live and resumed timestamps in the same format as session/list.
func sessionInfoPatch(title string, updatedAt time.Time) acp.SessionInfoUpdate {
	return acp.SessionInfoUpdate{Title: title, UpdatedAt: updatedAt.UTC().Format(time.RFC3339Nano)}
}

// newID returns a random UUIDv4.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
