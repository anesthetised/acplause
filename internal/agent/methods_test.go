package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

// testModels is a catalog with a plain and a reasoning model.
var testModels = []llm.Model{
	{ID: "m", ContextLength: 200_000},
	{ID: "r", Name: "Reasoner", SupportedParameters: []string{"tools", "reasoning"}},
	{ID: "no-tools", SupportedParameters: []string{"temperature"}},
}

// scriptedModel answers chat requests with queued functions, in order.
type scriptedModel chan func(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error)

func (m scriptedModel) Chat(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
	select {
	case fn := <-m:
		return fn(ctx, req, onDelta)
	default:
		return nil, errors.New("unexpected model request")
	}
}

func (m scriptedModel) reply(text string, finish string, usage *llm.Usage) {
	m <- func(_ context.Context, _ llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
		onDelta(llm.Delta{Kind: llm.DeltaText, Text: text})
		return &llm.Completion{Content: text, FinishReason: finish, Usage: usage}, nil
	}
}

func (m scriptedModel) callTool(id, name, args string) {
	m <- func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: id, Function: llm.FunctionCall{Name: name, Arguments: args}}}, FinishReason: llm.FinishToolCalls}, nil
	}
}

func (m scriptedModel) fail(err error) {
	m <- func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) { return nil, err }
}

// rpcResponse is a result or error the agent sent.
type rpcResponse struct {
	Result json.RawMessage
	Error  *jsonrpc.Error
}

// notification is a session/update the agent sent, decoded loosely.
type notification struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind       string          `json:"sessionUpdate"`
		State      string          `json:"state"`
		StopReason string          `json:"stopReason"`
		Status     string          `json:"status"`
		Error      *jsonrpc.Error  `json:"error"`
		Raw        json.RawMessage `json:"-"`
	} `json:"update"`
}

// acpClient drives an Agent over a jsonrpc.Conn, as an editor would.
type acpClient struct {
	t             *testing.T
	agent         *Agent
	in            *io.PipeWriter
	updates       chan notification
	elicitations  chan clientElicitation
	cancellations chan json.RawMessage

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse
	// answer chooses the permission outcome; nil allows.
	answer func(ctx context.Context) (outcome, option string)
}

// startAgent serves an Agent over pipes with a store in dir.
func startAgent(t *testing.T, dir string, model Model, list ListModels, wrapStore ...func(Store) Store) *acpClient {
	t.Helper()
	st, err := store.Open(dir, newID())
	if err != nil {
		t.Fatal(err)
	}
	clientOut, agentIn := io.Pipe()
	agentOut, clientIn := io.Pipe()
	conn := jsonrpc.NewConn(clientOut, clientIn, slog.New(slog.DiscardHandler))
	if list == nil {
		list = func(context.Context) ([]llm.Model, error) { return testModels, nil }
	}
	var persistence Store = st
	if len(wrapStore) > 0 {
		persistence = wrapStore[0](st)
	}
	c := &acpClient{t: t, in: agentIn, updates: make(chan notification, 256), pending: make(map[int]chan rpcResponse), elicitations: make(chan clientElicitation, 16), cancellations: make(chan json.RawMessage, 16)}
	c.agent = New(Config{Info: acp.Implementation{Name: "test", Version: "1"}, Store: persistence}, conn, model, list)

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = conn.Serve(context.Background(), c.agent.Methods())
	}()
	go c.read(agentOut)
	t.Cleanup(func() {
		_ = agentIn.Close()
		<-served
		c.agent.Shutdown()
		_ = conn.Close()
		_ = st.Close()
	})
	return c
}

func (c *acpClient) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *jsonrpc.Error  `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			panic(err)
		}
		switch {
		case msg.Method == acp.MethodSessionUpdate:
			var n notification
			_ = json.Unmarshal(msg.Params, &n)
			var raw struct {
				Update json.RawMessage `json:"update"`
			}
			_ = json.Unmarshal(msg.Params, &raw)
			n.Update.Raw = raw.Update
			c.updates <- n
		case msg.Method == acp.MethodElicitationCreate:
			c.elicitations <- clientElicitation{ID: *msg.ID, Params: msg.Params}
		case msg.Method == jsonrpc.CancelRequestMethod:
			c.cancellations <- msg.Params
		case msg.Method == acp.MethodSessionRequestPermission:
			go c.permit(*msg.ID)
		case msg.ID != nil:
			c.mu.Lock()
			ch := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.mu.Unlock()
			ch <- rpcResponse{Result: msg.Result, Error: msg.Error}
		}
	}
}

func (c *acpClient) permit(id int) {
	outcome, option := acp.OutcomeSelected, optionAllow
	c.mu.Lock()
	answer := c.answer
	c.mu.Unlock()
	if answer != nil {
		outcome, option = answer(context.Background())
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": map[string]any{"outcome": outcome, "optionId": option}}})
}

func (c *acpClient) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	_, _ = c.in.Write(append(b, '\n'))
}

// send issues a request and returns the channel its response arrives on.
func (c *acpClient) send(method string, params any) <-chan rpcResponse {
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return ch
}

func (c *acpClient) await(ch <-chan rpcResponse) rpcResponse {
	c.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		c.t.Fatal("no response")
		return rpcResponse{}
	}
}

// ok calls a method, requires success and decodes its result into out.
func (c *acpClient) ok(method string, params, out any) {
	c.t.Helper()
	r := c.await(c.send(method, params))
	if r.Error != nil {
		c.t.Fatalf("%s: %+v", method, r.Error)
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			c.t.Fatal(err)
		}
	}
}

// fails calls a method and requires an error with code.
func (c *acpClient) fails(method string, params any, code int) *jsonrpc.Error {
	c.t.Helper()
	r := c.await(c.send(method, params))
	if r.Error == nil || r.Error.Code != code {
		c.t.Fatalf("%s: got %s, %+v; want error %d", method, r.Result, r.Error, code)
	}
	return r.Error
}

func (c *acpClient) notify(method string, params any) {
	c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// untilIdle returns a session's updates up to its next idle state.
func (c *acpClient) untilIdle(sid string) []notification {
	c.t.Helper()
	var got []notification
	for {
		select {
		case n := <-c.updates:
			if n.SessionID != sid {
				continue
			}
			got = append(got, n)
			if n.Update.Kind == "state_update" && n.Update.State == string(acp.StateIdle) {
				return got
			}
		case <-time.After(10 * time.Second):
			c.t.Fatalf("no idle state for %s; got %+v", sid, got)
		}
	}
}

// waitFor returns the session's first update matching pred.
func (c *acpClient) waitFor(sid string, pred func(notification) bool) notification {
	c.t.Helper()
	for {
		select {
		case n := <-c.updates:
			if n.SessionID == sid && pred(n) {
				return n
			}
		case <-time.After(10 * time.Second):
			c.t.Fatalf("no matching update for %s", sid)
		}
	}
}

func (c *acpClient) initialize() {
	c.t.Helper()
	c.ok(acp.MethodInitialize, map[string]any{"protocolVersion": acp.ProtocolVersion, "info": map[string]any{"name": "test", "version": "1"}}, nil)
}

func (c *acpClient) newSession(cwd string) string {
	c.t.Helper()
	var res acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, map[string]any{"cwd": cwd, "mcpServers": []any{}}, &res)
	return res.SessionID
}

func (c *acpClient) selectModel(sid, model string) []acp.ConfigOption {
	c.t.Helper()
	var res struct {
		ConfigOptions []acp.ConfigOption `json:"configOptions"`
	}
	c.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": sid, "configId": configModel, "type": acp.ConfigValueID, "value": model}, &res)
	return res.ConfigOptions
}

func (c *acpClient) prompt(sid, text string) <-chan rpcResponse {
	return c.send(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": text}}})
}

func lastIdle(updates []notification) notification { return updates[len(updates)-1] }

func hasKind(updates []notification, kind string) bool {
	for _, u := range updates {
		if u.Update.Kind == kind {
			return true
		}
	}
	return false
}

func TestMethodsRequireInitialize(t *testing.T) {
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.fails(acp.MethodSessionNew, map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}, jsonrpc.CodeInvalidRequest)
	c.fails(acp.MethodInitialize, map[string]any{"protocolVersion": 1, "info": map[string]any{"name": "old", "version": "1"}}, acp.CodeUnsupportedProtocolVersion)
	var res acp.InitializeResponse
	c.ok(acp.MethodInitialize, map[string]any{"protocolVersion": acp.ProtocolVersion, "info": map[string]any{"name": "test", "version": "1"}}, &res)
	if res.ProtocolVersion != acp.ProtocolVersion || res.Info.Name != "test" || res.Capabilities.Session == nil {
		t.Fatalf("initialize: %+v", res)
	}
}

func TestSessionConfigOptions(t *testing.T) {
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)

	if opts := c.selectModel(sid, "r"); len(opts) != 3 || opts[1].ConfigID != configEffort {
		t.Fatalf("reasoning model options: %+v", opts)
	}
	set := func(id, value string) map[string]any {
		return map[string]any{"sessionId": sid, "configId": id, "type": acp.ConfigValueID, "value": value}
	}
	c.ok(acp.MethodSessionSetConfig, set(configEffort, "high"), nil)
	c.fails(acp.MethodSessionSetConfig, set(configEffort, "extreme"), jsonrpc.CodeInvalidParams)
	c.fails(acp.MethodSessionSetConfig, set(configModel, "no-tools"), jsonrpc.CodeInvalidParams)
	c.fails(acp.MethodSessionSetConfig, set("temperature", "1"), jsonrpc.CodeInvalidParams)
	c.fails(acp.MethodSessionSetConfig, map[string]any{"sessionId": sid, "configId": configModel, "type": "boolean", "value": true}, jsonrpc.CodeInvalidParams)
	c.fails(acp.MethodSessionSetConfig, map[string]any{"sessionId": "missing", "configId": configModel, "type": acp.ConfigValueID, "value": "m"}, acp.CodeResourceNotFound)

	// A new session starts with the last choice, effort included.
	var res acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, map[string]any{"cwd": cwd, "mcpServers": []any{}}, &res)
	if len(res.ConfigOptions) != 3 || res.ConfigOptions[0].CurrentValue != "r" || res.ConfigOptions[1].CurrentValue != "high" {
		t.Fatalf("remembered selection: %+v", res.ConfigOptions)
	}
	if opts := c.selectModel(sid, "m"); len(opts) != 2 {
		t.Fatalf("plain model options: %+v", opts)
	}
	c.fails(acp.MethodSessionSetConfig, set(configEffort, "low"), jsonrpc.CodeInvalidParams)
}

func TestNewSessionFailures(t *testing.T) {
	t.Run("catalog", func(t *testing.T) {
		c := startAgent(t, t.TempDir(), make(scriptedModel), func(context.Context) ([]llm.Model, error) {
			return nil, &llm.Error{Kind: llm.KindAuth, Status: 401, Message: "bad key"}
		})
		c.initialize()
		e := c.fails(acp.MethodSessionNew, map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}, acp.CodeAuthRequired)
		if !strings.Contains(e.Message, "bad key") {
			t.Fatalf("catalog error: %+v", e)
		}
	})
	t.Run("catalog recovers", func(t *testing.T) {
		var down atomic.Bool
		var calls atomic.Int32
		down.Store(true)
		c := startAgent(t, t.TempDir(), make(scriptedModel), func(context.Context) ([]llm.Model, error) {
			calls.Add(1)
			if down.Load() {
				return nil, &llm.Error{Kind: llm.KindServer, Status: 503, Message: "down"}
			}
			return testModels, nil
		})
		c.initialize()
		c.fails(acp.MethodSessionNew, map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}, jsonrpc.CodeInternalError)
		down.Store(false)
		c.newSession(t.TempDir())
		loaded := calls.Load()
		c.newSession(t.TempDir())
		if n := calls.Load(); n != loaded {
			t.Fatalf("a loaded catalog was listed again: %d calls, then %d", loaded, n)
		}
	})
	t.Run("no tool models", func(t *testing.T) {
		c := startAgent(t, t.TempDir(), make(scriptedModel), func(context.Context) ([]llm.Model, error) {
			return testModels[2:], nil
		})
		c.initialize()
		c.fails(acp.MethodSessionNew, map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}, jsonrpc.CodeInternalError)
	})
	t.Run("workspace", func(t *testing.T) {
		c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
		c.initialize()
		c.fails(acp.MethodSessionNew, map[string]any{"cwd": filepath.Join(t.TempDir(), "missing"), "mcpServers": []any{}}, jsonrpc.CodeInvalidParams)
	})
	t.Run("mcp", func(t *testing.T) {
		c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
		c.initialize()
		servers := []any{map[string]any{"type": "stdio", "name": "gone", "command": filepath.Join(t.TempDir(), "missing"), "args": []any{}, "env": []any{}}}
		if e := c.fails(acp.MethodSessionNew, map[string]any{"cwd": t.TempDir(), "mcpServers": servers}, jsonrpc.CodeInvalidParams); !strings.Contains(e.Message, "MCP") {
			t.Fatalf("MCP error: %+v", e)
		}
	})
}

func TestPromptOverProtocol(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)

	c.fails(acp.MethodSessionPrompt, map[string]any{"sessionId": "missing", "prompt": []any{map[string]any{"type": "text", "text": "hi"}}}, acp.CodeResourceNotFound)

	c.fails(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}}, jsonrpc.CodeInvalidParams)

	c.selectModel(sid, "m")
	model.callTool("w1", "write_file", `{"path":"a.txt","content":"hi"}`)
	model.reply("written", llm.FinishLength, &llm.Usage{PromptTokens: 10, CompletionTokens: 5, Cost: 0.25, HasPromptTokens: true, HasCompletionTokens: true})
	c.await(c.prompt(sid, "write a file"))
	updates := c.untilIdle(sid)
	if idle := lastIdle(updates); idle.Update.StopReason != string(acp.StopMaxTokens) {
		t.Fatalf("idle: %+v", idle.Update)
	}
	if !hasKind(updates, "usage_update") || !hasKind(updates, "tool_call_update") {
		t.Fatalf("missing tool or usage updates: %+v", updates)
	}
	if b, err := os.ReadFile(filepath.Join(cwd, "a.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("file not written: %q, %v", b, err)
	}

	for _, tc := range []struct {
		finish string
		stop   acp.StopReason
	}{
		{llm.FinishContentFilter, acp.StopRefusal},
		{llm.FinishError, acp.StopError},
	} {
		model.reply("x", tc.finish, nil)
		c.await(c.prompt(sid, "again"))
		if idle := lastIdle(c.untilIdle(sid)); idle.Update.StopReason != string(tc.stop) {
			t.Fatalf("%s: idle %+v", tc.finish, idle.Update)
		}
	}

	model.fail(&llm.Error{Kind: llm.KindRateLimited, Status: 429, Message: "slow down"})
	c.await(c.prompt(sid, "fail"))
	if idle := lastIdle(c.untilIdle(sid)); idle.Update.Error == nil || idle.Update.Error.Message != "slow down" {
		t.Fatalf("model error: %+v", idle.Update)
	}
}

func TestPermissionOutcomes(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")

	for _, tc := range []struct{ name, outcome, option string }{
		{"rejected", acp.OutcomeSelected, optionReject},
		{"dismissed", "cancelled", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.mu.Lock()
			c.answer = func(context.Context) (string, string) { return tc.outcome, tc.option }
			c.mu.Unlock()
			model.callTool("b1", "bash", `{"command":"touch ran"}`)
			model.reply("ok", llm.FinishStop, nil)
			c.await(c.prompt(sid, "run"))
			updates := c.untilIdle(sid)
			failed := false
			for _, u := range updates {
				failed = failed || u.Update.Status == string(acp.ToolFailed)
			}
			if !failed {
				t.Fatalf("tool not failed: %+v", updates)
			}
			if _, err := os.Stat(filepath.Join(cwd, "ran")); err == nil {
				t.Fatal("command ran without permission")
			}
		})
	}

	// Cancelling while the user is asked ends the turn as cancelled.
	asked := make(chan struct{})
	release := make(chan struct{})
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) {
		close(asked)
		<-release
		return acp.OutcomeSelected, optionAllow
	}
	c.mu.Unlock()
	model.callTool("b2", "bash", `{"command":"touch ran"}`)
	c.await(c.prompt(sid, "run"))
	<-asked
	c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
	idle := lastIdle(c.untilIdle(sid))
	close(release)
	if idle.Update.StopReason != string(acp.StopCancelled) {
		t.Fatalf("idle: %+v", idle.Update)
	}
}

func TestSessionListResumeCloseDelete(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	model.reply("hi there", llm.FinishStop, nil)
	c.await(c.prompt(sid, "greet me"))
	c.untilIdle(sid)

	var list acp.ListSessionsResponse
	c.ok(acp.MethodSessionList, map[string]any{"cwd": cwd}, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != sid || list.Sessions[0].Title == "" || list.NextCursor != "" {
		t.Fatalf("list: %+v", list)
	}
	c.fails(acp.MethodSessionList, map[string]any{"cursor": "!"}, jsonrpc.CodeInvalidParams)

	resume := func(cwd string, replay any) map[string]any {
		p := map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{}}
		if replay != nil {
			p["replayFrom"] = replay
		}
		return p
	}
	// Resuming an open session keeps it.
	c.ok(acp.MethodSessionResume, resume(cwd, nil), nil)
	c.untilIdle(sid)
	c.fails(acp.MethodSessionResume, resume(cwd, map[string]any{"type": "_later"}), jsonrpc.CodeInvalidParams)
	c.fails(acp.MethodSessionResume, resume(t.TempDir(), nil), jsonrpc.CodeInvalidParams)
	c.fails(acp.MethodSessionResume, map[string]any{"sessionId": "missing", "cwd": cwd, "mcpServers": []any{}}, acp.CodeResourceNotFound)

	c.ok(acp.MethodSessionClose, map[string]any{"sessionId": sid}, nil)
	c.fails(acp.MethodSessionClose, map[string]any{"sessionId": sid}, acp.CodeResourceNotFound)
	c.fails(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}}, acp.CodeResourceNotFound)
	c.fails(acp.MethodSessionResume, resume(t.TempDir(), nil), jsonrpc.CodeInvalidParams)

	// Resuming a closed session loads it from the store and replays it.
	c.ok(acp.MethodSessionResume, resume(cwd, map[string]any{"type": acp.ReplayFromStart}), nil)
	replayed := c.untilIdle(sid)
	if !hasKind(replayed, "user_message") || !hasKind(replayed, "agent_message") {
		t.Fatalf("replay: %+v", replayed)
	}
	model.reply("again", llm.FinishStop, nil)
	c.await(c.prompt(sid, "once more"))
	c.untilIdle(sid)

	c.ok(acp.MethodSessionDelete, map[string]any{"sessionId": sid}, nil)
	c.ok(acp.MethodSessionList, map[string]any{}, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("deleted session listed: %+v", list)
	}
	c.fails(acp.MethodSessionResume, resume(cwd, nil), acp.CodeResourceNotFound)
}

func TestListSessionsPages(t *testing.T) {
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.initialize()
	cwd := t.TempDir()
	for range listPageSize + 1 {
		c.newSession(cwd)
	}
	var first, second acp.ListSessionsResponse
	c.ok(acp.MethodSessionList, map[string]any{"cwd": cwd}, &first)
	if len(first.Sessions) != listPageSize || first.NextCursor == "" {
		t.Fatalf("first page: %d sessions, cursor %q", len(first.Sessions), first.NextCursor)
	}
	c.ok(acp.MethodSessionList, map[string]any{"cwd": cwd, "cursor": first.NextCursor}, &second)
	if len(second.Sessions) != 1 || second.NextCursor != "" {
		t.Fatalf("second page: %+v", second)
	}
}

func TestResumeTakesOverFromAnotherProcess(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	modelA, modelB := make(scriptedModel, 4), make(scriptedModel, 4)
	a := startAgent(t, dir, modelA, nil)
	a.initialize()
	sid := a.newSession(cwd)
	a.selectModel(sid, "m")

	b := startAgent(t, dir, modelB, nil)
	b.initialize()
	b.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{}}, nil)
	b.untilIdle(sid)
	b.selectModel(sid, "m") // A may not have saved its selection yet

	// The first process learns of the takeover when it next saves.
	if e := a.fails(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "mine?"}}}, jsonrpc.CodeInternalError); !strings.Contains(e.Message, store.ErrLost.Error()) {
		t.Fatalf("lost session: %+v", e)
	}

	// A busy session cannot be taken back.
	started := make(chan struct{})
	modelB <- func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if r := b.await(b.prompt(sid, "think")); r.Error != nil {
		t.Fatalf("prompt: %+v", r.Error)
	}
	<-started
	b.waitFor(sid, func(n notification) bool { return n.Update.State == string(acp.StateRunning) })
	a.fails(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{}}, jsonrpc.CodeInternalError)
	a.fails(acp.MethodSessionDelete, map[string]any{"sessionId": sid}, jsonrpc.CodeInternalError)
	b.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
	b.untilIdle(sid)
	b.notify(acp.MethodSessionCancel, map[string]any{"sessionId": "missing"})

	// drop is what lease renewal does on a takeover it detects.
	b.agent.drop(sid)
	b.fails(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "gone"}}}, jsonrpc.CodeInternalError)
}
