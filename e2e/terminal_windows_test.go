package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// denyShellLaunch makes the next real CreateProcess fail without deleting a
// directory held open by os.Root. The agent consumes its outer job's sole
// process slot; its command cannot start, even in a nested invocation job.
func denyShellLaunch(t *testing.T, a *agent) {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.stop(); _ = windows.CloseHandle(job) })
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	limits.BasicLimitInformation.ActiveProcessLimit = 1
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		t.Fatal(err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(a.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		t.Fatal(err)
	}
}

// windowsChildCommand creates a real native descendant with inherited job
// membership, not a mock terminal or a process launched by the test itself.
const windowsChildCommand = `$p = Start-Process -FilePath $env:ComSpec -ArgumentList @('/d','/c','ping -n 120 127.0.0.1 >NUL') -NoNewWindow -PassThru; [IO.File]::WriteAllText((Join-Path (Get-Location).Path 'child.pid'), [string]$p.Id); `

func windowsChildHandle(t *testing.T, path string) windows.Handle {
	t.Helper()
	waitForFile(t, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("child PID: %q %v", raw, err)
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.TerminateProcess(handle, 1); _ = windows.CloseHandle(handle) })
	return handle
}

func TestCancelDuringPowerShell(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	args, _ := json.Marshal(map[string]string{"command": windowsChildCommand + `$p.WaitForExit()`})
	llm.enqueue(callTool("powershell", string(args)), reply("ok"))
	a, sid, cwd := started(t, llm, withEnv("ACPLAUSE_BASH_TIMEOUT=0", "ACPLAUSE_MAX_ROUNDS=0"))
	a.accepted(a.prompt(sid, "run it"))
	a.toolStarted(sid)
	child := windowsChildHandle(t, filepath.Join(cwd, "child.pid"))
	a.notify("session/cancel", map[string]any{"sessionId": sid})
	updates := a.untilIdle(sid)
	want := []string{"tool_call_update cancelled", "state_update idle cancelled"}
	got := kinds(updates)
	if len(got) < 2 || !slices.Equal(got[len(got)-2:], want) || len(filter(updates, "tool_call_update")) != 1 {
		t.Fatalf("updates after cancel: %v", got)
	}
	if state, err := windows.WaitForSingleObject(child, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("child survived cancellation: state=%d err=%v", state, err)
	}
	a.accepted(a.prompt(sid, "next"))
	a.untilIdle(sid)
	msgs := llm.request(1).Messages
	if result := msgs[3]; result["role"] != "tool" || result["content"] != "Cancelled by the user." {
		t.Fatalf("history after cancel: %v", msgs)
	}
}

func TestPowerShellJobEndsOnAgentDeath(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	args, _ := json.Marshal(map[string]string{"command": windowsChildCommand + `$p.WaitForExit()`})
	llm.enqueue(callTool("powershell", string(args)))
	a, sid, cwd := started(t, llm)
	a.accepted(a.prompt(sid, "run it"))
	a.toolStarted(sid)
	child := windowsChildHandle(t, filepath.Join(cwd, "child.pid"))
	a.stopOnce.Do(func() {
		if err := a.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = a.stdin.Close()
		_ = a.cmd.Wait()
		<-a.readerDone
	})
	if state, err := windows.WaitForSingleObject(child, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("child survived agent death: state=%d err=%v", state, err)
	}
}
