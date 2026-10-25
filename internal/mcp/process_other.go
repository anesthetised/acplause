//go:build !unix && !windows

package mcp

import "os/exec"

func startProcess(cmd *exec.Cmd) (*processControl, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processControl{
		signal: func(bool) { _ = cmd.Process.Kill() },
		close:  func() error { return nil },
	}, nil
}
