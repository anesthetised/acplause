package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
)

var shellSpec = Spec{
	Name:        shellName,
	Kind:        acp.ToolExecute,
	Description: "Run a " + shellName + " command in the working directory and return its combined stdout and stderr. Use it for builds, tests, git and other shell work. Commands are killed on cancellation or when the configured timeout is reached.",
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "` + shellCommandDescription + `"}
		},
		"required": ["command"]
	}`),
}

type bashArgs struct {
	Command string `json:"command"`
}

func (tb *Toolbox) bash(args bashArgs) (*Call, error) {
	if err := required("command", args.Command); err != nil {
		return nil, err
	}
	call := &Call{
		shellLimit: tb.opts.OutputLimit,
		Title:      "Run " + clip(firstLine(args.Command), 80),
		Command:    args.Command,
	}
	call.run = func(ctx context.Context) (string, error) {
		return runShell(ctx, tb.roots.Cwd(), args.Command, tb.opts, call.shell)
	}
	return call, nil
}

var errTimeout = errors.New("command timed out")

// runShell owns a process group on Unix and a job on Windows. Unix descendants
// that detach into another group can survive cleanup; neither is a sandbox.
func runShell(ctx context.Context, dir, command string, opts Options, observer *ShellObserver) (string, error) {
	runCtx := ctx
	if *opts.BashTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeoutCause(ctx, *opts.BashTimeout, errTimeout)
		defer cancel()
	}

	out := observer
	if out == nil {
		out = &ShellObserver{output: cappedBuffer{limit: opts.OutputLimit}}
	}
	cmd := shellCommand(runCtx, command)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = time.Second // don't wait on pipes held open by stray children
	cleanup, err := startShell(cmd)
	if err == nil {
		out.mu.Lock()
		out.started, out.command, out.cwd = true, command, dir
		out.wake()
		out.mu.Unlock()
		err = cmd.Wait()
		cleanupErr := cleanup()
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil // the shell exited 0; only a stray child held the output open
		}
		err = errors.Join(err, cleanupErr)
		out.mu.Lock()
		status := shellExit(cmd.ProcessState)
		out.exit = &status
		out.wake()
		out.mu.Unlock()
	}
	switch {
	case ctx.Err() != nil:
		return out.text(), ctx.Err()
	case errors.Is(context.Cause(runCtx), errTimeout):
		return out.text(), fmt.Errorf("timed out after %s", *opts.BashTimeout)
	}
	return out.text(), err
}

// ShellExit preserves real process status; neither field means unknown completion.
type ShellExit struct {
	Code   *uint32
	Signal string
}

// ShellSnapshot is a detached observation of one invocation, retaining raw bytes.
// Output starts at the offset passed to Snapshot; Dropped counts bytes past the cap.
type ShellSnapshot struct {
	Started      bool
	Command, Cwd string
	Output       []byte
	Dropped      int
	Exit         *ShellExit
}

// ShellObserver holds bounded invocation-local state. Reading it is optional:
// a slow or abandoned consumer cannot stop pipe draining or process cleanup.
type ShellObserver struct {
	mu           sync.Mutex
	output       cappedBuffer
	started      bool
	command, cwd string
	exit         *ShellExit
	changed      chan struct{}
}

// Changed coalesces start, output and exit wakeups; consumers read Snapshot
// after a wakeup, since multiple writes may share a single notification.
func (o *ShellObserver) Changed() <-chan struct{} { return o.changed }

// Snapshot copies retained output from offset without exposing writer memory.
func (o *ShellObserver) Snapshot(offset int) ShellSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	var exit *ShellExit
	if o.exit != nil {
		v := *o.exit
		if v.Code != nil {
			v.Code = new(*v.Code)
		}
		exit = &v
	}
	return ShellSnapshot{Started: o.started, Command: o.command, Cwd: o.cwd,
		Output: slices.Clone(o.output.data[min(max(offset, 0), len(o.output.data)):]), Dropped: o.output.dropped, Exit: exit}
}

// Write only retains a bounded prefix and wakes the consumer; it never calls
// consumer code or waits on publication. exec serializes combined pipe writes.
func (o *ShellObserver) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.output.Write(p)
	o.wake()
	return n, err
}

func (o *ShellObserver) wake() {
	select {
	case o.changed <- struct{}{}:
	default:
	}
}

func (o *ShellObserver) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.output.String()
}

// cappedBuffer keeps the first limit bytes written and counts the rest.
type cappedBuffer struct {
	data    []byte
	limit   int
	dropped int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := min(len(p), b.limit-len(b.data))
	b.data = append(b.data, p[:n]...)
	b.dropped += len(p) - n
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	s := strings.ToValidUTF8(string(b.data), "�")
	if b.dropped > 0 {
		s += fmt.Sprintf("\n... (%d more bytes of output dropped)", b.dropped)
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
