// Package tools implements the tools acplause offers the model. File tools
// are confined to the session's workspace roots; bash runs in the working
// directory without confinement.
package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
)

// DefaultBashTimeout allows long builds and test suites to finish.
const DefaultBashTimeout = 30 * time.Minute

// Options tunes tool limits. Zero values select defaults.
type Options struct {
	BashTimeout *time.Duration // nil selects DefaultBashTimeout; zero disables the deadline
	OutputLimit int            // bytes of command output kept; default 50 000
}

func (o Options) withDefaults() Options {
	if o.BashTimeout == nil {
		o.BashTimeout = new(DefaultBashTimeout)
	}
	o.OutputLimit = cmp.Or(o.OutputLimit, 50_000)
	return o
}

// Spec declares a tool to the model.
type Spec struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema of the arguments
	Kind        acp.ToolKind
}

// Call is a validated tool invocation, ready to run.
type Call struct {
	shell      *ShellObserver // opt-in, attached before Run
	shellLimit int
	Title      string
	Locations  []string // absolute paths the call touches
	Command    string   // the command line, for bash
	run        func(ctx context.Context) (string, error)
	preview    func(ctx context.Context) (*acp.ToolCallContent, error) // file tools
	diff       *acp.ToolCallContent                                    // set by run before it writes, for file tools
}

// ObserveShell enables bounded observation of a bash call. Attach it before Run;
// non-shell calls return nil. Each prepared call owns its observer.
func (c *Call) ObserveShell() *ShellObserver {
	if c.Command == "" {
		return nil
	}
	if c.shell == nil {
		c.shell = &ShellObserver{output: cappedBuffer{limit: c.shellLimit}, changed: make(chan struct{}, 1)}
	}
	return c.shell
}

// Preview returns the diff a file tool would apply, without applying it, so
// the user can approve it first. Other tools have no preview. An error means
// the call would fail; like Run's, it is meant for the model.
func (c *Call) Preview(ctx context.Context) (*acp.ToolCallContent, error) {
	if c.preview == nil {
		return nil, nil
	}
	return c.preview(ctx)
}

// Result is what a call reports back to the model.
type Result struct {
	Output string
	Failed bool
	Diff   *acp.ToolCallContent // the change a successful file tool made
}

// Run executes the call. When ctx is cancelled the call stops as soon as it
// can and its result should be discarded.
func (c *Call) Run(ctx context.Context) Result {
	out, err := c.run(ctx)
	if err == nil {
		return Result{Output: cmp.Or(out, "(no output)"), Diff: c.diff}
	}
	if out != "" {
		out += "\n"
	}
	return Result{Output: out + "Error: " + err.Error(), Failed: true}
}

// Toolbox is the set of tools of one session.
type Toolbox struct {
	roots *Roots
	opts  Options
	tools []tool
}

type tool struct {
	Spec
	prepare func(args json.RawMessage) (*Call, error)
}

// New opens a toolbox over the roots [cwd, extra...].
func New(cwd string, extra []string, opts Options) (*Toolbox, error) {
	roots, err := OpenRoots(append([]string{cwd}, extra...)...)
	if err != nil {
		return nil, err
	}
	tb := &Toolbox{roots: roots, opts: opts.withDefaults()}
	tb.tools = []tool{
		newTool(readFileSpec, tb.readFile),
		newTool(listDirSpec, tb.listDir),
		newTool(searchCodeSpec, tb.searchCode),
		newTool(getInstructionsSpec, tb.getInstructions),
		newTool(writeFileSpec, tb.writeFile),
		newTool(editSpec, tb.edit),
		newTool(editBatchSpec, tb.editBatch),
		newTool(bashSpec, tb.bash),
	}
	return tb, nil
}

// Close releases the workspace roots.
func (tb *Toolbox) Close() error { return tb.roots.Close() }

// Specs lists the tools in a stable order.
func (tb *Toolbox) Specs() []Spec {
	specs := make([]Spec, len(tb.tools))
	for i, t := range tb.tools {
		specs[i] = t.Spec
	}
	return specs
}

// Kind returns the display kind of a tool, or acp.ToolOther if unknown.
func (tb *Toolbox) Kind(name string) acp.ToolKind {
	if t := tb.lookup(name); t != nil {
		return t.Kind
	}
	return acp.ToolOther
}

// Prepare validates a call's JSON arguments. Errors are meant for the
// model: unknown tools, malformed JSON and invalid arguments.
func (tb *Toolbox) Prepare(name, args string) (*Call, error) {
	t := tb.lookup(name)
	if t == nil {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return t.prepare(json.RawMessage(args))
}

func (tb *Toolbox) lookup(name string) *tool {
	for i := range tb.tools {
		if tb.tools[i].Name == name {
			return &tb.tools[i]
		}
	}
	return nil
}

// newTool binds a spec to a preparer of typed arguments.
func newTool[A any](spec Spec, prepare func(A) (*Call, error)) tool {
	return tool{Spec: spec, prepare: func(raw json.RawMessage) (*Call, error) {
		if len(bytes.TrimSpace(raw)) == 0 {
			raw = json.RawMessage("{}")
		}
		var args A
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments for %s: %v", spec.Name, err)
		}
		return prepare(args)
	}}
}

func required(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}
