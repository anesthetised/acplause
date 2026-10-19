package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/tools"
)

// deps is what sessions share with the agent.
type deps struct {
	out       Client
	model     Model
	maxRounds int
	tools     tools.Options
	log       *slog.Logger
}

// Session is one conversation. Prompts queue up and run one turn at a time
// on a worker goroutine that exists only while there is work.
type Session struct {
	id  string
	cwd string
	// instructions is AGENTS.md as read when the session was created.
	instructions string
	deps         deps
	catalog      *catalog

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc

	mu            sync.Mutex
	dirs          []string
	toolbox       *tools.Toolbox
	activeToolbox *tools.Toolbox // retained by the single worker until its current tool returns
	model         string
	effort        string
	title         string
	updatedAt     time.Time
	cost          float64         // cumulative USD
	history       []llm.Message   // what the model sees
	transcript    []acp.Update    // what the client saw, for replay
	state         acp.StateUpdate // last reported foreground state
	queue         []*submission   // prompts not yet inserted
	worker        chan struct{}   // closed when the running worker exits; nil when idle
	stopTurn      context.CancelFunc
	closed        bool
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
	instructions, err := toolbox.Instructions(parent)
	if err != nil {
		d.log.Warn("skipping AGENTS.md", "cwd", cwd, "err", err)
	}
	ctx, cancel := context.WithCancel(parent)
	return &Session{
		id:           newID(),
		cwd:          cwd,
		instructions: instructions,
		deps:         d,
		catalog:      c,
		ctx:          ctx,
		cancel:       cancel,
		dirs:         dirs,
		toolbox:      toolbox,
		effort:       effortDefault,
		updatedAt:    time.Now(),
		history:      []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt(cwd, dirs, instructions)}},
		state:        acp.StateUpdate{State: acp.StateIdle},
	}, nil
}

// Submit queues a prompt. The reply resolves with the user message's id
// once it is inserted, or fails with Request cancelled if the prompt is
// cancelled first, through ctx or session/cancel.
func (s *Session) Submit(ctx context.Context, prompt []acp.ContentBlock) *jsonrpc.Reply {
	sub := &submission{ctx: ctx, prompt: prompt, reply: jsonrpc.NewReply()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil {
		sub.reply.Reject(acp.ErrRequestCancelled())
		return sub.reply
	}
	if s.model == "" {
		sub.reply.Reject(acp.ErrInvalidParams("select a model before submitting a prompt"))
		return sub.reply
	}
	s.queue = append(s.queue, sub)
	sub.stopWatch = context.AfterFunc(ctx, func() { s.withdraw(sub) })
	if s.worker == nil {
		s.worker = make(chan struct{})
		go s.work(s.worker)
	}
	return sub.reply
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
	if worker != nil {
		<-worker
	}
	s.cancel()
	s.closeToolbox(s.toolbox)
}

// Resume reattaches the client: it replaces the additional directories,
// optionally replays the history, and reports the current state.
func (s *Session) Resume(dirs []string, replay bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Equal(dirs, s.dirs) {
		toolbox, err := tools.New(s.cwd, dirs, s.deps.tools)
		if err != nil {
			return err
		}
		old := s.toolbox
		s.toolbox, s.dirs = toolbox, dirs
		s.history[0].Content = systemPrompt(s.cwd, dirs, s.instructions)
		if old != s.activeToolbox {
			s.closeToolbox(old)
		}
	}
	if replay {
		for _, u := range s.transcript {
			s.send(u)
		}
	}
	s.send(s.state)
	return nil
}

// acquireToolbox pins the current roots for one tool, not for an entire
// model request. Resume can change the roots while the model is streaming.
func (s *Session) acquireToolbox() *tools.Toolbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeToolbox = s.toolbox
	return s.toolbox
}

func (s *Session) releaseToolbox(tb *tools.Toolbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeToolbox = nil
	if tb != s.toolbox {
		s.closeToolbox(tb)
	}
}

func (s *Session) closeToolbox(tb *tools.Toolbox) {
	if err := tb.Close(); err != nil {
		s.deps.log.Warn("closing workspace", "session", s.id, "err", err)
	}
}

// Info describes the session for session/list.
func (s *Session) Info() acp.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return acp.SessionInfo{
		SessionID:             s.id,
		Cwd:                   s.cwd,
		AdditionalDirectories: s.dirs,
		Title:                 s.title,
		UpdatedAt:             s.updatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// ConfigOptions lists the session's settings.
func (s *Session) ConfigOptions() []acp.ConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalog.options(s.model, s.effort)
}

// SetConfigOption changes a setting; the next model request uses it.
func (s *Session) SetConfigOption(id, valueType string, raw json.RawMessage) error {
	var value string
	if valueType != acp.ConfigValueID || json.Unmarshal(raw, &value) != nil {
		return acp.ErrInvalidParams("%s takes an id value", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch id {
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

// runTurn inserts the prompt and runs foreground work until it ends.
func (s *Session) runTurn(ctx context.Context, sub *submission) {
	if !s.insert(ctx, sub) {
		return
	}
	s.setState(acp.StateUpdate{State: acp.StateRunning})
	stop, err := s.converse(ctx)
	if ctx.Err() != nil {
		stop, err = acp.StopCancelled, nil
	}
	if err != nil {
		s.deps.log.Warn("turn failed", "session", s.id, "err", err)
	}
	s.setState(acp.StateUpdate{State: acp.StateIdle, StopReason: stop, Error: err})
}

// insert adds the user message to the conversation and acknowledges the
// prompt, unless it was cancelled first. The prompt response precedes the
// user_message update on the wire.
func (s *Session) insert(ctx context.Context, sub *submission) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || sub.ctx.Err() != nil {
		sub.reply.Reject(acp.ErrRequestCancelled())
		return false
	}
	msg := acp.UserMessage{MessageID: newID(), Content: sub.prompt}
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: promptText(sub.prompt)})
	s.transcript = append(s.transcript, msg)
	first := s.title == ""
	if first {
		s.title = title(sub.prompt)
	}
	s.updatedAt = time.Now()
	sub.reply.Resolve(acp.PromptResponse{MessageID: msg.MessageID})
	s.send(msg)
	if first {
		// Not in the transcript: replay is conversation history, and
		// session/list already reports the title.
		s.send(acp.SessionInfoUpdate{Title: s.title, UpdatedAt: s.updatedAt.UTC().Format(time.RFC3339Nano)})
	}
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
