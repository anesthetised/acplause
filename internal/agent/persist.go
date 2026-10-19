package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

// Store persists sessions; *store.Store implements it (ADR 0004).
type Store interface {
	Create(ctx context.Context, sess store.Session) error
	Save(ctx context.Context, sess store.Session, history, transcript []store.Row) error
	Acquire(ctx context.Context, id string) (*store.Loaded, error)
	Renew(ctx context.Context, ids []string) (lost []string, err error)
	Release(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, cwd string, offset, limit int) ([]store.Session, bool, error)
}

// interrupted is the result of tool calls a dead process left unfinished.
const interrupted = "acplause exited before this tool call finished."

// entry is a transcript update as stored, with its kind.
type entry struct {
	Kind   string          `json:"kind"`
	Update json.RawMessage `json:"update"`
}

func encodeEntry(u acp.Update) ([]byte, error) {
	var kind string
	switch u.(type) {
	case acp.UserMessage:
		kind = "user_message"
	case acp.AgentMessage:
		kind = "agent_message"
	case acp.AgentThought:
		kind = "agent_thought"
	case acp.ToolCallUpdate:
		kind = "tool_call_update"
	default:
		return nil, fmt.Errorf("cannot store a %T", u)
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return nil, err
	}
	return json.Marshal(entry{Kind: kind, Update: raw})
}

func decodeEntry(data []byte) (acp.Update, error) {
	var e entry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	var u acp.Update
	var err error
	switch e.Kind {
	case "user_message":
		var m acp.UserMessage
		err = json.Unmarshal(e.Update, &m)
		u = m
	case "agent_message":
		var m acp.AgentMessage
		err = json.Unmarshal(e.Update, &m)
		u = m
	case "agent_thought":
		var m acp.AgentThought
		err = json.Unmarshal(e.Update, &m)
		u = m
	case "tool_call_update":
		var m acp.ToolCallUpdate
		err = json.Unmarshal(e.Update, &m)
		u = m
	default:
		return nil, fmt.Errorf("unknown stored update %q", e.Kind)
	}
	return u, err
}

// record describes the session for the store. Requires s.mu.
func (s *Session) record() store.Session {
	return store.Session{
		ID: s.id, Cwd: s.cwd, Dirs: s.dirs, Title: s.title, UpdatedAt: s.updatedAt,
		Model: s.model, Effort: s.effort, Cost: s.cost, State: string(s.state.State),
	}
}

// pending collects what changed since the last save and marks it saved.
// Requires s.mu; undo restores the marks if the save fails.
func (s *Session) pending() (sess store.Session, history, transcript []store.Row, undo func(), err error) {
	from, dirty := s.saved, s.dirty
	for i, m := range s.history[1+s.saved:] { // the system prompt is rebuilt, not stored
		data, err := json.Marshal(m)
		if err != nil {
			return sess, nil, nil, nil, err
		}
		history = append(history, store.Row{Seq: from + i, Data: data})
	}
	for _, i := range slices.Sorted(maps.Keys(dirty)) {
		if s.transcript[i] == nil {
			continue // a message that has not started streaming
		}
		data, err := encodeEntry(s.transcript[i])
		if err != nil {
			return sess, nil, nil, nil, err
		}
		transcript = append(transcript, store.Row{Seq: i, Data: data})
	}
	s.saved, s.dirty = len(s.history)-1, map[int]bool{}
	undo = func() {
		s.saved = min(s.saved, from)
		maps.Copy(s.dirty, dirty)
	}
	return s.record(), history, transcript, undo, nil
}

// flush saves what changed. Saves are serialised and each one takes the
// latest state, so they cannot reorder. A session lost to another process
// stops saving; other failures are logged and retried by the next flush.
func (s *Session) flush() {
	if s.deps.store == nil {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if s.lost {
		s.mu.Unlock()
		return
	}
	sess, history, transcript, undo, err := s.pending()
	s.mu.Unlock()
	if err == nil {
		err = s.deps.store.Save(context.WithoutCancel(s.ctx), sess, history, transcript)
	}
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, store.ErrLost) {
		s.lost = true
		return
	}
	if undo != nil {
		undo()
	}
	s.deps.log.Warn("saving session", "session", s.id, "err", err)
}

// flushAsync saves without blocking a handler on the read loop.
func (s *Session) flushAsync() { go s.flush() }

// restoreSession rebuilds a session from the store. A session its last
// process left mid-turn is repaired so every tool call has a result.
func restoreSession(parent context.Context, d deps, c *catalog, l *store.Loaded) (*Session, error) {
	s, err := newSession(parent, d, c, l.Cwd, l.Dirs)
	if err != nil {
		return nil, err
	}
	s.id, s.title, s.updatedAt, s.cost = l.ID, l.Title, l.UpdatedAt, l.Cost
	if c.has(l.Model) {
		s.model = l.Model
		if c.reasons(l.Model) && validEffort(l.Effort) {
			s.effort = l.Effort
		}
	}
	for _, data := range l.History {
		var m llm.Message
		if err := json.Unmarshal(data, &m); err != nil {
			s.Close()
			return nil, fmt.Errorf("session %s history: %w", l.ID, err)
		}
		s.history = append(s.history, m)
	}
	for _, data := range l.Transcript {
		u, err := decodeEntry(data)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("session %s transcript: %w", l.ID, err)
		}
		s.transcript = append(s.transcript, u)
	}
	s.saved = len(l.History)
	if l.State != string(acp.StateIdle) {
		s.repair()
	}
	return s, nil
}

// repair finishes what a dead process left: unfinished tool calls fail and
// get a result in the history.
func (s *Session) repair() {
	for i, u := range s.transcript {
		if call, ok := u.(acp.ToolCallUpdate); ok && (call.Status == acp.ToolPending || call.Status == acp.ToolInProgress) {
			s.transcript[i] = call.Merge(acp.ToolCallUpdate{
				Status:    acp.ToolFailed,
				Content:   []acp.ToolCallContent{acp.ToolText(interrupted)},
				RawOutput: jsonString(interrupted),
			})
			s.dirty[i] = true
		}
	}
	answered := map[string]bool{}
	for _, m := range s.history {
		if m.Role == llm.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range slices.Clone(s.history) {
		for _, call := range m.ToolCalls {
			if !answered[call.ID] {
				s.history = append(s.history, toolResult(call.ID, interrupted))
			}
		}
	}
}

func errSessionBusy() *jsonrpc.Error {
	return jsonrpc.NewError(jsonrpc.CodeInternalError, "%v", store.ErrBusy)
}

func errSessionLost() *jsonrpc.Error {
	return jsonrpc.NewError(jsonrpc.CodeInternalError, "%v", store.ErrLost)
}

// markLost stops the session from saving: another process owns it now.
func (s *Session) markLost() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lost = true
}

func (s *Session) isLost() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost
}
