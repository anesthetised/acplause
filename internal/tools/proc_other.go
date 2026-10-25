//go:build !unix

package tools

import "os"

// shellExit reports the real exit code; Windows has no Unix signal status.
func shellExit(state *os.ProcessState) ShellExit {
	if state != nil {
		if code := state.ExitCode(); code >= 0 {
			return ShellExit{Code: new(uint32(code))}
		}
	}
	return ShellExit{}
}
