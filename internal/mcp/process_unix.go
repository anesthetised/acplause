//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func startProcess(cmd *exec.Cmd) (*processControl, error) {
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processControl{
		signal: func(kill bool) { signalProcess(cmd, kill) },
		close:  func() error { return nil },
	}, nil
}
func signalProcess(cmd *exec.Cmd, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
