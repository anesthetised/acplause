package tools

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf16"

	"github.com/anesthetised/acplause/internal/winproc"
	"golang.org/x/sys/windows"
)

const shellName = "powershell"
const shellCommandDescription = "The command line in Windows PowerShell 5.1 syntax (not bash), without profiles or interactive prompts"

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	// Use the OS installation, not a workspace/PATH lookalike. A missing system
	// shell remains a launch error; it never falls back to another interpreter.
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		cmd := exec.CommandContext(ctx, "powershell.exe")
		cmd.Err = err
		return cmd
	}
	// Preserve quotes, newlines and Unicode in a UTF-16LE payload. Decode it
	// under -Command: PowerShell 5.1's -EncodedCommand forces CLIXML stderr
	// even with -OutputFormat Text. Suppress noninteractive progress as well.
	script := "$ProgressPreference = 'SilentlyContinue'; [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); $OutputEncoding = [Console]::OutputEncoding;\n" + command +
		// -Command otherwise reduces native failure codes to 1. Test $?
		// immediately, so a successful command after a failure still exits zero.
		"\nif (-not $?) { if ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; exit 1 }\n"
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[i*2:], unit)
	}
	// The launcher is ASCII and contains no double quotes. Only Base64 data
	// enters its single-quoted literal, avoiding another layer of script quoting.
	launcher := "& ([ScriptBlock]::Create([Text.Encoding]::Unicode.GetString([Convert]::FromBase64String('" + base64.StdEncoding.EncodeToString(data) + "'))))"
	return exec.CommandContext(ctx, filepath.Join(dir, "WindowsPowerShell", "v1.0", "powershell.exe"),
		"-NoLogo", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-Command", launcher)
}

func startShell(ctx context.Context, cmd *exec.Cmd) (*shellProcess, error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	in, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer w.Close()
	job, err := winproc.Start(ctx, winproc.Config{Path: cmd.Path, Args: cmd.Args, Dir: cmd.Dir, Env: cmd.Environ(), Files: [3]*os.File{in, w, w}})
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	// A single pipe preserves the combined byte stream without serialization.
	drained := make(chan error, 1)
	go func() { _, err := io.Copy(cmd.Stdout, r); drained <- err }()
	return &shellProcess{
		wait: func() (ShellExit, error) { state, err := job.Wait(); return shellExit(state), err },
		close: func() error {
			defer r.Close()
			err := job.Close()
			timer := time.NewTimer(cmd.WaitDelay)
			defer timer.Stop()
			select {
			case copyErr := <-drained:
				return errors.Join(err, copyErr)
			case <-timer.C:
				// A broker outside the job can retain an inherited pipe. Match
				// exec.WaitDelay: retain the prefix without blocking completion.
				_ = r.Close()
				<-drained
				return err
			}
		},
	}, nil
}
