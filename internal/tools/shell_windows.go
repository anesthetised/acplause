package tools

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"os/exec"
	"path/filepath"
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

func startShell(cmd *exec.Cmd) (func() error, error) {
	job, err := winproc.Start(cmd)
	if err != nil {
		return nil, err
	}
	return job.Close, nil
}
