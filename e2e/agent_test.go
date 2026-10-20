// Package e2e runs the acplause binary over stdio against a scripted LLM
// server and validates everything it writes against the ACP v2 schema.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const timeout = 10 * time.Second

var binary string
var mcpBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "acplause-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "acplause")
	args := []string{"build", "-o", binary}
	if raceEnabled {
		args = append(args, "-race")
	}
	build := exec.Command("go", append(args, "../cmd/acplause")...)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building acplause:", err)
		os.Exit(1)
	}
	mcpBinary = filepath.Join(dir, "mcp-fixture")
	build = exec.Command("go", "build", "-o", mcpBinary, "./testdata/mcp")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building MCP fixture:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// message is one line of agent output.
type message struct {
	seq    int             // line number in the output
	batch  bool            // part of a batch response
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// update is a decoded session/update notification.
type update struct {
	TerminalID      string          `json:"terminalId"`
	Command         string          `json:"command"`
	Cwd             string          `json:"cwd"`
	Output          *terminalOutput `json:"output"`
	ExitStatus      *terminalExit   `json:"exitStatus"`
	Data            string          `json:"data"`
	seq             int
	SessionID       string
	SessionUpdate   string          `json:"sessionUpdate"`
	CompactionID    string          `json:"compactionId"`
	Summary         json.RawMessage `json:"summary"`
	CompactionError string          `json:"-"`
	MessageID       string          `json:"messageId"`
	State           string          `json:"state"`
	StopReason      string          `json:"stopReason"`
	Error           *rpcError       `json:"error"`
	Content         json.RawMessage `json:"content"`
	ToolCallID      string          `json:"toolCallId"`
	Status          string          `json:"status"`
	Name            string          `json:"name"`
	Title           string          `json:"title"`
	Severity        string          `json:"severity"`
	Description     string          `json:"description"`
	UpdatedAt       string          `json:"updatedAt"`
	Kind            string          `json:"kind"`
	RawInput        json.RawMessage `json:"rawInput"`
	Plan            *planSnapshot   `json:"plan"`
	Locations       []struct {
		Path string `json:"path"`
	} `json:"locations"`
	Used uint64 `json:"used"`
	Size uint64 `json:"size"`
	Cost *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost"`
}

// text joins the text in the update's content, whatever its shape.
func (u update) text() string {
	type block struct {
		Text    string `json:"text"`
		Content *block `json:"content"`
	}
	var blocks []block
	if json.Unmarshal(u.Content, &blocks) != nil {
		var one block
		_ = json.Unmarshal(u.Content, &one)
		blocks = []block{one}
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Content != nil {
			bl = *bl.Content
		}
		b.WriteString(bl.Text)
	}
	return b.String()
}

func (u update) is(kind string) bool { return u.SessionUpdate == kind }

func (u update) String() string {
	switch u.SessionUpdate {
	case "state_update":
		return strings.TrimSpace("state_update " + u.State + " " + u.StopReason)
	case "tool_call_update":
		return "tool_call_update " + u.Status
	}
	return u.SessionUpdate
}

// agent is a running acplause process.
type agent struct {
	t          *testing.T
	llm        *mockLLM
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stderr     *bytes.Buffer
	out        chan *message
	readerDone chan struct{}

	// askUser leaves permission requests to the test; by default they are
	// allowed as they arrive, like a client that auto-approves.
	askUser bool

	stopOnce sync.Once
	writeMu  sync.Mutex // the test and auto-approval both write to stdin
	mu       sync.Mutex
	methods  map[string]string // method of each request sent, by id
	nextID   int
	pending  []*message // read but not yet taken
}

type option func(*agent)

func withEnv(kv ...string) option {
	return func(a *agent) { a.cmd.Env = append(a.cmd.Env, kv...) }
}

// withDataDir makes agents share a session store, as processes of one user
// do. Later environment entries win.
func withDataDir(dir string) option { return withEnv("ACPLAUSE_DATA_DIR=" + dir) }

// askingUser makes the test answer permission requests itself.
func askingUser(a *agent) { a.askUser = true }

func start(t *testing.T, llm *mockLLM, opts ...option) *agent {
	t.Helper()
	v, err := loadValidator()
	if err != nil {
		t.Fatalf("loading schema: %v", err)
	}

	cmd := exec.Command(binary)
	cmd.Env = append(cleanEnv(),
		"ACPLAUSE_BASE_URL="+llm.srv.URL,
		"ACPLAUSE_API_KEY=test-key",
		"ACPLAUSE_LOG_LEVEL=debug",
		"ACPLAUSE_DATA_DIR="+t.TempDir(), // withDataDir shares one between agents
	)
	a := &agent{
		t: t, llm: llm, cmd: cmd,
		stderr:     &bytes.Buffer{},
		out:        make(chan *message, 1024),
		readerDone: make(chan struct{}),
		methods:    map[string]string{},
	}
	for _, opt := range opts {
		opt(a)
	}
	cmd.Stderr = &lockedWriter{w: a.stderr}
	if a.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go a.read(stdout, v)
	t.Cleanup(a.stop)
	return a
}

func cleanEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "ACPLAUSE_") || strings.HasPrefix(kv, "OPENROUTER_")
	})
}

// stop closes stdin, which must make the agent exit cleanly. It runs once;
// later calls, such as the cleanup after a test stopped the agent, do
// nothing.
func (a *agent) stop() { a.stopOnce.Do(a.exit) }

func (a *agent) exit() {
	_ = a.stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- a.cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			a.t.Errorf("agent exited with %v", err)
		}
	case <-time.After(timeout):
		_ = a.cmd.Process.Kill()
		a.t.Error("agent did not exit after stdin closed")
	}
	<-a.readerDone
	if a.t.Failed() {
		a.t.Logf("agent stderr:\n%s", a.stderr)
	}
}

// read decodes and validates each line of output.
func (a *agent) read(stdout io.Reader, v *validator) {
	defer close(a.readerDone)
	defer close(a.out)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(nil, 16<<20)
	for seq := 0; sc.Scan(); seq++ {
		line := bytes.Clone(sc.Bytes())
		var batch []json.RawMessage
		if json.Unmarshal(line, &batch) != nil {
			a.accept(v, &message{seq: seq}, line)
			continue
		}
		for _, entry := range batch {
			a.accept(v, &message{seq: seq, batch: true}, entry)
		}
	}
}

func (a *agent) accept(v *validator, msg *message, raw []byte) {
	if err := json.Unmarshal(raw, msg); err != nil {
		a.t.Errorf("agent wrote invalid JSON: %q", raw)
		return
	}
	a.mu.Lock()
	method := a.methods[string(msg.ID)]
	a.mu.Unlock()
	if err := v.validate(raw, msg, method); err != nil {
		a.t.Errorf("schema violation: %v\n%s", err, raw)
	}
	if msg.Method == "session/request_permission" && !a.askUser {
		// Not a.answer: this runs off the test goroutine, and stdin may
		// already be closed when the test is done.
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
			"outcome": map[string]any{"outcome": "selected", "optionId": optionOfKind(a.t, msg, "allow_once")},
		}})
		a.writeMu.Lock()
		_, _ = a.stdin.Write(append(b, '\n'))
		a.writeMu.Unlock()
	}
	a.out <- msg
}

// permissionRequest takes the next session/request_permission.
func (a *agent) permissionRequest() *message {
	a.t.Helper()
	return a.take(func(m *message) bool { return m.Method == "session/request_permission" })
}

// answer responds to a request the agent sent.
func (a *agent) answer(req *message, result any) {
	a.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// optionOfKind returns the id of the permission option of a kind.
func optionOfKind(t *testing.T, req *message, kind string) string {
	var p struct {
		Options []struct{ OptionID, Kind string }
	}
	_ = json.Unmarshal(req.Params, &p)
	for _, o := range p.Options {
		if o.Kind == kind {
			return o.OptionID
		}
	}
	t.Errorf("no %s option in %s", kind, req.Params)
	return ""
}

// send writes a raw line to the agent.
func (a *agent) send(line string) {
	a.t.Helper()
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if _, err := io.WriteString(a.stdin, line+"\n"); err != nil {
		a.t.Fatalf("write to agent: %v", err)
	}
}

func (a *agent) write(v any) {
	a.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		a.t.Fatal(err)
	}
	a.send(string(b))
}

// request sends a request and returns its id.
func (a *agent) request(method string, params any) string {
	a.t.Helper()
	a.mu.Lock()
	a.nextID++
	id := strconv.Itoa(a.nextID)
	a.methods[id] = method
	a.mu.Unlock()
	a.write(map[string]any{"jsonrpc": "2.0", "id": a.nextID, "method": method, "params": params})
	return id
}

func (a *agent) notify(method string, params any) {
	a.t.Helper()
	a.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends a request and waits for its response.
func (a *agent) call(method string, params any) *message {
	a.t.Helper()
	return a.response(a.request(method, params))
}

// ok calls a method that must succeed and decodes its result.
func (a *agent) ok(method string, params, result any) *message {
	a.t.Helper()
	resp := a.call(method, params)
	if resp.Error != nil {
		a.t.Fatalf("%s failed: %+v", method, resp.Error)
	}
	if result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			a.t.Fatal(err)
		}
	}
	return resp
}

// fails calls a method that must fail with code.
func (a *agent) fails(method string, params any, code int) *rpcError {
	a.t.Helper()
	resp := a.call(method, params)
	if resp.Error == nil || resp.Error.Code != code {
		a.t.Fatalf("%s: got result %s, error %+v; want error %d", method, resp.Result, resp.Error, code)
	}
	return resp.Error
}

func (a *agent) response(id string) *message {
	a.t.Helper()
	return a.take(func(m *message) bool { return m.Method == "" && string(m.ID) == id })
}

// take returns the first unread message matching pred, reading more output
// as needed. Messages it skips stay available to later takes.
func (a *agent) take(pred func(*message) bool) *message {
	a.t.Helper()
	for i, m := range a.pending {
		if pred(m) {
			a.pending = slices.Delete(a.pending, i, i+1)
			return m
		}
	}
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-a.out:
			if !ok {
				a.t.Fatal("agent output ended")
			}
			if pred(m) {
				return m
			}
			a.pending = append(a.pending, m)
		case <-deadline:
			a.t.Fatalf("timed out; unread output: %s", a.describePending())
		}
	}
}

// expectQuiet asserts that nothing matching pred arrives for a short while.
func (a *agent) expectQuiet(pred func(*message) bool) {
	a.t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case m := <-a.out:
			if pred(m) {
				a.t.Fatalf("unexpected message: %+v", m)
			}
			a.pending = append(a.pending, m)
		case <-deadline:
			return
		}
	}
}

func (a *agent) describePending() string {
	var parts []string
	for _, m := range a.pending {
		b, _ := json.Marshal(m)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n")
}

// nextUpdate takes the next session/update for session sid.
func (a *agent) nextUpdate(sid string) update {
	a.t.Helper()
	m := a.take(func(m *message) bool { return m.Method == "session/update" && sessionOf(m) == sid })
	return decodeUpdate(a.t, m)
}

// updateWhere takes the first session/update for sid matching pred.
func (a *agent) updateWhere(sid string, pred func(update) bool) update {
	a.t.Helper()
	m := a.take(func(m *message) bool {
		return m.Method == "session/update" && sessionOf(m) == sid && pred(decodeUpdate(a.t, m))
	})
	return decodeUpdate(a.t, m)
}

// toolStarted takes the session's updates up to the next tool call that
// starts running, past any permission request for it, and returns that call.
func (a *agent) toolStarted(sid string) update {
	a.t.Helper()
	for {
		if u := a.nextUpdate(sid); u.is("tool_call_update") && u.Status == "in_progress" {
			return u
		}
	}
}

// untilIdle takes the session's updates up to and including the next idle
// state update.
func (a *agent) untilIdle(sid string) []update {
	a.t.Helper()
	var updates []update
	for {
		u := a.nextUpdate(sid)
		updates = append(updates, u)
		if u.is("state_update") && u.State == "idle" {
			return updates
		}
	}
}

func sessionOf(m *message) string {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(m.Params, &p)
	return p.SessionID
}

func decodeUpdate(t *testing.T, m *message) update {
	t.Helper()
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	u := update{seq: m.seq, SessionID: p.SessionID}
	type plain update
	raw := struct {
		*plain
		Error json.RawMessage `json:"error"`
	}{plain: (*plain)(&u)}
	if err := json.Unmarshal(p.Update, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Error) > 0 {
		var err error
		if u.is("compaction_update") {
			err = json.Unmarshal(raw.Error, &u.CompactionError)
		} else {
			err = json.Unmarshal(raw.Error, &u.Error)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return u
}

// Conveniences for the common flow.

func (a *agent) initialize() {
	a.t.Helper()
	a.ok("initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "e2e", "version": "1"}}, nil)
}

type newSessionResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []configOption `json:"configOptions"`
}

type configOption struct {
	ConfigID     string `json:"configId"`
	Category     string `json:"category"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value string `json:"value"`
		Name  string `json:"name"`
	} `json:"options"`
}

func (a *agent) newSession(cwd string, extra ...string) newSessionResult {
	a.t.Helper()
	var res newSessionResult
	params := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	if len(extra) > 0 {
		params["additionalDirectories"] = extra
	}
	a.ok("session/new", params, &res)
	return res
}

// prompt sends a text prompt and returns the request id.
func (a *agent) prompt(sid, text string) string {
	a.t.Helper()
	return a.request("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	})
}

// accepted waits for a prompt's successful response and returns messageId.
func (a *agent) accepted(id string) (string, *message) {
	a.t.Helper()
	resp := a.response(id)
	var res struct {
		MessageID string `json:"messageId"`
	}
	if resp.Error != nil || json.Unmarshal(resp.Result, &res) != nil || res.MessageID == "" {
		a.t.Fatalf("prompt not accepted: %+v %s", resp.Error, resp.Result)
	}
	return res.MessageID, resp
}

// started initialises the agent and opens a session in a fresh directory.
func started(t *testing.T, llm *mockLLM, opts ...option) (*agent, string, string) {
	t.Helper()
	a := start(t, llm, opts...)
	a.initialize()
	cwd := t.TempDir()
	sid := a.newSession(cwd).SessionID
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
	return a, sid, cwd
}

func kinds(updates []update) []string {
	var out []string
	for _, u := range updates {
		if len(out) > 0 && out[len(out)-1] == u.String() && strings.HasSuffix(u.SessionUpdate, "_chunk") {
			continue // collapse runs of chunks
		}
		out = append(out, u.String())
	}
	return out
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
