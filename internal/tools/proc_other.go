//go:build !unix

package tools

import (
	"os"
	"os/exec"
)

// killProcessGroupOnCancel keeps exec's default of killing only the shell.
func killProcessGroupOnCancel(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) {}

func shellExit(state *os.ProcessState) ShellExit {
	if state != nil {
		if code := state.ExitCode(); code >= 0 {
			return ShellExit{Code: new(uint32(code))}
		}
	}
	return ShellExit{}
}
