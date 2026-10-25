// Package winproc owns Windows subprocesses with private kill-on-close jobs.
package winproc

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job retains a non-inheritable handle so owner death terminates its members.
// It is a lifecycle mechanism, not a sandbox against same-user processes.
type Job struct {
	mu     sync.Mutex
	handle windows.Handle
}

// Start launches cmd suspended and assigns it to a job before any user code
// runs. Failure kills and reaps the suspended process rather than running it
// without ownership. The caller must Wait on cmd and Close the returned job.
func Start(cmd *exec.Cmd) (*Job, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process job: %w", err)
	}
	j := &Job{handle: handle}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(handle, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("configure process job: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW
	if cmd.Cancel != nil {
		cmd.Cancel = j.Terminate
	}
	// exec's context watcher starts inside cmd.Start. Serialize cancellation
	// with assignment, or it could terminate an empty job and miss the process
	// that we assign and resume immediately afterwards.
	j.mu.Lock()
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(handle)
		j.handle = 0
		j.mu.Unlock()
		return nil, err
	}
	err = j.assignAndResume(cmd.Process.Pid)
	j.mu.Unlock()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.Join(err, j.Close())
	}
	return j, nil
}

func (j *Job) assignAndResume(pid int) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open suspended process: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(j.handle, process); err != nil {
		return fmt.Errorf("assign process job: %w", err)
	}
	// os/exec retains the process handle but closes the initial thread handle.
	// CREATE_SUSPENDED prevents user code from creating threads or descendants,
	// so a thread snapshot can find the initial thread before resuming it.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("find suspended thread: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("open suspended thread: %w", err)
		}
		defer windows.CloseHandle(thread)
		count, err := windows.ResumeThread(thread)
		if err != nil {
			return fmt.Errorf("resume process: %w", err)
		}
		if count != 1 {
			return fmt.Errorf("unexpected initial thread suspend count: %d", count)
		}
		return nil
	}
	return fmt.Errorf("suspended process thread unavailable: %w", err)
}

// Terminate kills the job rather than just its original process. Keeping the
// handle open allows Close to observe job membership after termination.
func (j *Job) Terminate() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return nil
	}
	return windows.TerminateJobObject(j.handle, 1)
}

// Close terminates remaining members and waits for the job's active count to
// reach zero before releasing the handle. A bounded failure is reported; the
// kill-on-close limit still applies when releasing a job after an error.
func (j *Job) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return nil
	}
	err := windows.TerminateJobObject(j.handle, 1)
	if err == nil {
		err = j.waitEmpty()
	}
	err = errors.Join(err, windows.CloseHandle(j.handle))
	j.handle = 0
	return err
}

func (j *Job) waitEmpty() error {
	// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION's fixed-width Windows layout.
	var accounting struct {
		TotalUserTime, TotalKernelTime, PeriodUserTime, PeriodKernelTime int64
		PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := windows.QueryInformationJobObject(j.handle, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return fmt.Errorf("observe process job cleanup: %w", err)
		}
		if accounting.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("process job cleanup timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
