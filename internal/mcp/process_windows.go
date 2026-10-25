package mcp

import (
	"context"
	"os"
	"os/exec"

	"github.com/anesthetised/acplause/internal/winproc"
)

func startProcess(cmd *exec.Cmd, in, out *os.File) (*processControl, error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	job, err := winproc.Start(context.Background(), winproc.Config{Path: cmd.Path, Args: cmd.Args, Dir: cmd.Dir, Env: cmd.Environ(), Files: [3]*os.File{in, out, null}})
	if err != nil {
		return nil, err
	}
	return &processControl{
		// The stdin-close grace period is handled by stdioTransport. Windows
		// has no SIGTERM equivalent here; either stop phase terminates the job.
		signal: func(bool) { _ = job.Terminate() },
		close:  job.Close,
		wait:   func() error { _, err := job.Wait(); return err },
	}, nil
}
