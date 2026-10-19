//go:build unix

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// shellExit reports signals separately: exec's -1 is not an exit code.
func shellExit(state *os.ProcessState) ShellExit {
	if state == nil {
		return ShellExit{}
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		signal := status.Signal()
		// Go's Signal.String uses descriptions on Unix; ACP needs conventional names.
		names := map[syscall.Signal]string{
			syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
			syscall.SIGILL: "SIGILL", syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT",
			syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
			syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
			syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM",
			syscall.SIGXCPU: "SIGXCPU", syscall.SIGXFSZ: "SIGXFSZ", syscall.SIGSYS: "SIGSYS",
			syscall.SIGPROF: "SIGPROF", syscall.SIGVTALRM: "SIGVTALRM", syscall.SIGIO: "SIGIO",
			syscall.SIGURG: "SIGURG", syscall.SIGWINCH: "SIGWINCH", syscall.SIGCHLD: "SIGCHLD",
			syscall.SIGCONT: "SIGCONT", syscall.SIGSTOP: "SIGSTOP", syscall.SIGTSTP: "SIGTSTP",
			syscall.SIGTTIN: "SIGTTIN", syscall.SIGTTOU: "SIGTTOU",
		}
		if name := names[signal]; name != "" {
			return ShellExit{Signal: name}
		}
		return ShellExit{Signal: signal.String()}
	}
	if code := state.ExitCode(); code >= 0 {
		return ShellExit{Code: new(uint32(code))}
	}
	return ShellExit{}
}
