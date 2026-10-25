package winproc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	if mode == "inspect" {
		cwd, err := os.Getwd()
		if err != nil {
			os.Exit(5)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[3:], "cwd": cwd, "marker": os.Getenv("ACPLAUSE_JOB_MARKER")})
		os.Exit(0)
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
	if mode == "supervisor" || mode == "creating" {
		var boundary func(uint32)
		if mode == "creating" {
			boundary = func(pid uint32) {
				if err := os.WriteFile(os.Getenv("ACPLAUSE_JOB_READY"), []byte(strconv.FormatUint(uint64(pid), 10)), 0o600); err != nil {
					os.Exit(6)
				}
				time.Sleep(time.Minute)
			}
		}
		job, err := start(context.Background(), jobConfig(t, cmd), boundary)
		if err != nil {
			os.Exit(3)
		}
		defer job.Close()
		_, _ = job.Wait()
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

func jobConfig(t *testing.T, cmd *exec.Cmd) Config {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })
	files := [3]*os.File{null, null, null}
	if f, ok := cmd.Stdin.(*os.File); ok {
		files[0] = f
	}
	if f, ok := cmd.Stdout.(*os.File); ok {
		files[1] = f
	}
	if f, ok := cmd.Stderr.(*os.File); ok {
		files[2] = f
	}
	return Config{Path: cmd.Path, Args: cmd.Args, Dir: cmd.Dir, Env: cmd.Environ(), Files: files}
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
			job, err := Start(t.Context(), jobConfig(t, cmd))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = job.Close() })
			worker := waitForWorker(t, ready)
			if mode == "exit" {
				if _, err := job.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
			if mode != "exit" {
				_, _ = job.Wait()
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
	job, err := Start(ctx, jobConfig(t, base))
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	worker := waitForWorker(t, ready)
	cancel()
	if _, err := job.Wait(); err == nil {
		t.Fatal("cancelled command succeeded")
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	assertTerminated(t, worker)
}

func TestJobSupervisorDeath(t *testing.T) {
	for _, mode := range []string{"supervisor", "creating"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			cmd := helperCommand(t, mode, ready)
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
		})
	}
}

func TestJobRejectsFailedLaunch(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing.exe"))
	if job, err := Start(t.Context(), jobConfig(t, cmd)); err == nil || job != nil {
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
	if job, err := Start(ctx, jobConfig(t, cmd)); err == nil || job != nil || cmd.Process != nil {
		t.Fatalf("cancelled launch: job=%v process=%v err=%v", job, cmd.Process, err)
	}
}

func TestJobStdioArgumentsEnvironment(t *testing.T) {
	cmd := helperCommand(t, "inspect", "")
	args := []string{"with space", `quote"test`, `C:\trail\`, "Привет 🚀", "", "line\nbreak"}
	cmd.Args = append(cmd.Args, "--")
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(cmd.Env, "ACPLAUSE_JOB_MARKER=native-env-Привет")
	cmd.Dir = filepath.Join(t.TempDir(), "dir with space Привет")
	if err := os.Mkdir(cmd.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd.Stdout = w
	job, err := Start(t.Context(), jobConfig(t, cmd))
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	_ = w.Close()
	state, err := job.Wait()
	if err != nil || state.ExitCode() != 0 {
		t.Fatalf("process status: %v %v", state, err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Args        []string
		Cwd, Marker string
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("output %q: %v", data, err)
	}
	if !slices.Equal(args, result.Args) || result.Marker != "native-env-Привет" {
		t.Fatalf("process inputs: %+v", result)
	}
	actual, err := os.Stat(result.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.Stat(cmd.Dir)
	if err != nil || !os.SameFile(actual, expected) {
		t.Fatalf("process cwd: %q %v", result.Cwd, err)
	}
}

func TestJobCancelledAtCreation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var child windows.Handle
	var openErr error
	job, err := start(ctx, jobConfig(t, helperCommand(t, "worker", filepath.Join(t.TempDir(), "ready"))), func(pid uint32) {
		child, openErr = windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
		cancel()
	})
	if child != 0 {
		defer windows.CloseHandle(child)
		defer windows.TerminateProcess(child, 1)
	}
	if openErr != nil {
		t.Fatal(openErr)
	}
	if job != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup: job=%v err=%v", job, err)
	}
	assertTerminated(t, child)
}
