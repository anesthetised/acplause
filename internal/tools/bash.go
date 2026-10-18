package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
)

var bashSpec = Spec{
	Name:        "bash",
	Kind:        acp.ToolExecute,
	Description: "Run a bash command in the working directory and return its combined stdout and stderr. Use it for builds, tests, git and other shell work. Commands are killed after a timeout.",
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The command line, run with bash -c"}
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
	return &Call{
		Title: "Run " + clip(firstLine(args.Command), 80),
		run: func(ctx context.Context) (string, error) {
			return runShell(ctx, tb.roots.Cwd(), args.Command, tb.opts)
		},
	}, nil
}

var errTimeout = errors.New("command timed out")

// runShell runs command in its own process group, so that cancellation and
// timeouts kill everything it started.
func runShell(ctx context.Context, dir, command string, opts Options) (string, error) {
	runCtx, cancel := context.WithTimeoutCause(ctx, opts.BashTimeout, errTimeout)
	defer cancel()

	out := &cappedBuffer{limit: opts.OutputLimit}
	cmd := exec.CommandContext(runCtx, "bash", "-c", command)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = time.Second // don't wait on pipes held open by stray children
	killProcessGroupOnCancel(cmd)

	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return out.String(), ctx.Err()
	case errors.Is(context.Cause(runCtx), errTimeout):
		return out.String(), fmt.Errorf("timed out after %s", opts.BashTimeout)
	}
	return out.String(), err
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
