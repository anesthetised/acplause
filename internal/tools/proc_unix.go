//go:build unix

package tools

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func startShell(_ context.Context, cmd *exec.Cmd) (*shellProcess, error) {
	killProcessGroupOnCancel(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &shellProcess{
		wait:  func() (ShellExit, error) { err := cmd.Wait(); return shellExit(cmd.ProcessState), err },
		close: func() error { killProcessGroup(cmd); return nil },
	}, nil
}

func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// killProcessGroup signals descendants left in the shell's group after it exits.
// A descendant that changed groups is outside this cleanup boundary.
func killProcessGroup(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

// shellExit reports signals separately: exec's -1 is not an exit code.
func shellExit(state *os.ProcessState) ShellExit {
	if state == nil {
		return ShellExit{}
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		signal := status.Signal()
		// Go's Signal.String uses descriptions on Unix; ACP needs conventional names.
		if name := unix.SignalName(signal); name != "" {
			return ShellExit{Signal: name}
		}
		return ShellExit{Signal: signal.String()}
	}
	if code := state.ExitCode(); code >= 0 {
		return ShellExit{Code: new(uint32(code))}
	}
	return ShellExit{}
}
