package winproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestJobHelper runs only as an explicitly selected subprocess. No shell is
// involved, so job ownership is tested independently of PowerShell behavior.
func TestJobHelper(t *testing.T) {
	mode := os.Getenv("ACPLAUSE_JOB_HELPER")
	if mode == "" {
		return
	}
	if mode == "worker" {
		if err := os.WriteFile(os.Getenv("ACPLAUSE_JOB_READY"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	cmd := helperCommand(t, "worker", os.Getenv("ACPLAUSE_JOB_READY"))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if mode == "supervisor" {
		job, err := Start(cmd)
		if err != nil {
			os.Exit(3)
		}
		defer job.Close()
		_ = cmd.Wait()
		os.Exit(0)
	}
	if err := cmd.Start(); err != nil {
		os.Exit(4)
	}
	if mode == "exit" {
		os.Exit(0)
	}
	_ = cmd.Wait()
	os.Exit(0)
}

func helperCommand(t *testing.T, mode, ready string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestJobHelper$")
	cmd.Env = append(os.Environ(), "ACPLAUSE_JOB_HELPER="+mode, "ACPLAUSE_JOB_READY="+ready)
	return cmd
}

func waitForWorker(t *testing.T, ready string) windows.Handle {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ready)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil && pid > 0 {
				handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = windows.TerminateProcess(handle, 1); _ = windows.CloseHandle(handle) })
				return handle
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker did not start")
	return 0
}

func assertTerminated(t *testing.T, handle windows.Handle) {
	t.Helper()
	state, err := windows.WaitForSingleObject(handle, 5000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("worker still alive: state=%d err=%v", state, err)
	}
}

func TestJobClosesDescendants(t *testing.T) {
	for _, mode := range []string{"wait", "exit"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			cmd := helperCommand(t, mode, ready)
			job, err := Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = job.Close() })
			worker := waitForWorker(t, ready)
			if mode == "exit" {
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
			if mode != "exit" {
				_ = cmd.Wait()
			}
			assertTerminated(t, worker)
			if err := job.Close(); err != nil {
				t.Fatal("second close:", err)
			}
		})
	}
}

func TestJobCancellation(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := helperCommand(t, "wait", ready)
	cmd := exec.CommandContext(ctx, base.Path, base.Args[1:]...)
	cmd.Env = base.Env
	job, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	worker := waitForWorker(t, ready)
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("cancelled command succeeded")
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	assertTerminated(t, worker)
}

func TestJobSupervisorDeath(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := helperCommand(t, "supervisor", ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	worker := waitForWorker(t, ready)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	assertTerminated(t, worker)
}

func TestJobRejectsFailedLaunch(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing.exe"))
	if job, err := Start(cmd); err == nil || job != nil {
		t.Fatalf("failed launch: job=%v err=%v", job, err)
	}
}

func TestJobRejectsCancelledLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestJobHelper$")
	if job, err := Start(cmd); err == nil || job != nil || cmd.Process != nil {
		t.Fatalf("cancelled launch: job=%v process=%v err=%v", job, cmd.Process, err)
	}
}
