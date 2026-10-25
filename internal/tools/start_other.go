//go:build !unix && !windows

package tools

import (
	"context"
	"os/exec"
)

func startShell(_ context.Context, cmd *exec.Cmd) (*shellProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &shellProcess{
		wait:  func() (ShellExit, error) { err := cmd.Wait(); return shellExit(cmd.ProcessState), err },
		close: func() error { return nil },
	}, nil
}
