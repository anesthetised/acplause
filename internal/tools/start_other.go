//go:build !unix && !windows

package tools

import "os/exec"

func startShell(cmd *exec.Cmd) (func() error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() error { return nil }, nil
}
