package mcp

import (
	"os/exec"

	"github.com/anesthetised/acplause/internal/winproc"
)

func startProcess(cmd *exec.Cmd) (*processControl, error) {
	job, err := winproc.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &processControl{
		// The stdin-close grace period is handled by stdioTransport. Windows
		// has no SIGTERM equivalent here; either stop phase terminates the job.
		signal: func(bool) { _ = job.Terminate() },
		close:  job.Close,
	}, nil
}
