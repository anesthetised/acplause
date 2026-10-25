package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPowerShellNativeCommands(t *testing.T) {
	tb, err := New(t.TempDir(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	for _, spec := range tb.Specs() {
		if spec.Name == "bash" {
			t.Fatal("Windows must not advertise bash syntax")
		}
	}
	for _, tc := range []struct {
		name, command, output string
		failed                bool
		code                  uint32
	}{
		{"unicode", `[Console]::Write('Привет 🚀 & < > " quotes'); [Console]::Error.Write(' stderr')`, `Привет 🚀 & < > " quotes stderr`, false, 0},
		{"multiline", "[Console]::Write('one')\n[Console]::Write('two')", "onetwo", false, 0},
		{"progress", `Write-Progress -Activity 'test' -Status 'test' -PercentComplete 50; [Console]::Write('done')`, "done", false, 0},
		{"native exit", `& $env:ComSpec /d /c exit 7`, "", true, 7},
		{"success after native failure", `& $env:ComSpec /d /c exit 7; [Console]::Write('ok')`, "ok", false, 0},
		{"cmdlet failure", `Write-Error 'sentinel'`, "", true, 1},
		{"explicit exit", `exit 9`, "", true, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := tb.bash(bashArgs{Command: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			observer := call.ObserveShell()
			result := call.Run(context.Background())
			if strings.Contains(result.Output, "#< CLIXML") {
				t.Fatalf("non-text shell output: %q", result.Output)
			}
			if result.Failed != tc.failed || tc.output != "" && result.Output != tc.output {
				t.Fatalf("result: %+v", result)
			}
			exit := observer.Snapshot(0).Exit
			if exit == nil || exit.Code == nil {
				t.Fatalf("missing exit code: %+v", exit)
			}
			if *exit.Code != tc.code || exit.Signal != "" {
				t.Fatalf("exit code=%d signal=%q, want code=%d; output=%q", *exit.Code, exit.Signal, tc.code, result.Output)
			}
		})
	}
}

func TestPowerShellTimeout(t *testing.T) {
	timeout := 100 * time.Millisecond
	output, err := runShell(context.Background(), t.TempDir(), "Start-Sleep -Seconds 60", Options{BashTimeout: &timeout, OutputLimit: 100}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}
