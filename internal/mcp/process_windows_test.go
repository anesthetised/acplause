package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestMCPWindowsJobHelper(t *testing.T) {
	mode := os.Getenv("ACPLAUSE_WIN_MCP_HELPER")
	if mode == "" {
		return
	}
	if mode == "child" {
		if err := os.WriteFile(os.Getenv("ACPLAUSE_WIN_MCP_READY"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestMCPWindowsJobHelper$")
	cmd.Env = append(os.Environ(), "ACPLAUSE_WIN_MCP_HELPER=child")
	cmd.Stdout = os.Stdout // deliberately keep the transport pipe open
	if cmd.Start() != nil {
		os.Exit(3)
	}
	if mode == "exit" {
		os.Exit(0)
	}
	time.Sleep(time.Minute) // deliberately ignore stdin and block large writes
	os.Exit(0)
}

func TestMCPWindowsJobCleanup(t *testing.T) {
	for _, mode := range []string{"blocked", "exit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ready := filepath.Join(dir, "ready")
			exe, _ := os.Executable()
			p := newPeer(context.Background())
			tr, err := startStdio(p, Config{Command: exe, Args: []string{"-test.run=^TestMCPWindowsJobHelper$"}, Env: map[string]string{
				"ACPLAUSE_WIN_MCP_HELPER": mode, "ACPLAUSE_WIN_MCP_READY": ready,
			}}, dir)
			if err != nil {
				t.Fatal(err)
			}
			p.t = tr
			defer tr.Close()
			var child windows.Handle
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				raw, _ := os.ReadFile(ready)
				pid, _ := strconv.Atoi(string(raw))
				if pid > 0 {
					child, err = windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
					if err != nil {
						t.Fatal(err)
					}
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if child == 0 {
				t.Fatal("MCP descendant did not start")
			}
			defer windows.CloseHandle(child)
			defer windows.TerminateProcess(child, 1)
			if mode == "blocked" {
				tr.start()
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				var result any
				if err := p.call(ctx, "tools/call", map[string]string{"padding": strings.Repeat("x", 1<<20)}, &result); err == nil {
					t.Fatal("non-reading MCP server answered")
				}
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			if state, err := windows.WaitForSingleObject(child, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
				t.Fatalf("MCP descendant survived: state=%d err=%v", state, err)
			}
		})
	}
}
