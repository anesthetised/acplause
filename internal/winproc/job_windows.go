// Package winproc owns Windows subprocesses with private kill-on-close jobs.
package winproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job retains a non-inheritable handle so owner death terminates its members.
// It is a lifecycle mechanism, not a sandbox against same-user processes.
type Job struct {
	mu         sync.Mutex
	handle     windows.Handle
	process    *os.Process
	stopCancel func() bool
}

// Start creates a process atomically inside its private job. The caller must
// Wait and Close the returned Job; neither context cancellation nor owner death
// can leave a child created before job membership was established.
func Start(ctx context.Context, cfg Config) (*Job, error) {
	return start(ctx, cfg, nil)
}

// afterCreate lets lifecycle tests stop exactly after CreateProcessW, before
// the suspended child is resumed. Production has no launch-boundary callback.
func start(ctx context.Context, cfg Config, afterCreate func(uint32)) (*Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	j.process, err = createProcess(ctx, cfg, handle, afterCreate)
	if err != nil {
		return nil, errors.Join(err, j.Close())
	}
	j.stopCancel = context.AfterFunc(ctx, func() { _ = j.Terminate() })
	return j, nil
}

// Wait reaps the direct process and reports its actual status, using the same
// exit-error shape as os/exec. Closing the job remains the caller's obligation.
func (j *Job) Wait() (*os.ProcessState, error) {
	state, err := j.process.Wait()
	j.stopCancel()
	if err == nil && !state.Success() {
		err = &exec.ExitError{ProcessState: state}
	}
	return state, err
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
	if j.stopCancel != nil {
		j.stopCancel()
	}
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
