//go:build !windows

package tools

import (
	"context"
	"os/exec"
)

const shellName = "bash"
const shellCommandDescription = "The command line, run with bash -c"

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "bash", "-c", command)
}
