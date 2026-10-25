package winproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Config describes one native process without os/exec's unexported startup
// machinery, which cannot pass PROC_THREAD_ATTRIBUTE_JOB_LIST.
type Config struct {
	Path  string      // absolute executable path; extensions resolve with exec.LookPath
	Args  []string    // argv including argv[0]; nil selects Path alone
	Dir   string      // initial working directory
	Env   []string    // complete environment, nil inherits; use exec.Cmd.Environ to deduplicate
	Files [3]*os.File // stdin, stdout, stderr; all must be open
}

// PROC_THREAD_ATTRIBUTE_JOB_LIST is in the Windows 10 SDK, but absent from the
// pinned x/sys constants. Its value is ProcThreadAttributeValue(13,0,1,0).
const procThreadAttributeJobList = 0x0002000d

func createProcess(ctx context.Context, cfg Config, job windows.Handle, afterCreate func(uint32)) (*os.Process, error) {
	if !filepath.IsAbs(cfg.Path) {
		return nil, errors.New("process executable path must be absolute")
	}
	path, err := exec.LookPath(cfg.Path)
	if err != nil {
		return nil, err
	}
	app, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	args := cfg.Args
	if len(args) == 0 {
		args = []string{path}
	}
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return nil, err
	}
	var dir *uint16
	if cfg.Dir != "" {
		dir, err = windows.UTF16PtrFromString(cfg.Dir)
		if err != nil {
			return nil, err
		}
	}
	env := cfg.Env
	if env == nil {
		env = os.Environ()
	}
	block := make([]uint16, 0)
	for _, entry := range env {
		value, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, errors.New("invalid process environment")
		}
		block = append(block, value...)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	block = append(block, 0)
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, err
	}
	defer attributes.Delete()
	if err := attributes.Update(procThreadAttributeJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return nil, fmt.Errorf("configure atomic process job: %w", err)
	}
	var handles [3]windows.Handle
	defer func() {
		for _, handle := range handles {
			if handle != 0 {
				_ = windows.CloseHandle(handle)
			}
		}
	}()
	for i, file := range cfg.Files {
		if file == nil {
			return nil, errors.New("process requires stdin, stdout and stderr files")
		}
		if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(), &handles[i], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, fmt.Errorf("duplicate process stream: %w", err)
		}
	}
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), unsafe.Sizeof(handles)); err != nil {
		return nil, err
	}
	startup := windows.StartupInfoEx{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.StdInput, startup.StdOutput, startup.StdErr = handles[0], handles[1], handles[2]
	startup.ProcThreadAttributeList = attributes.List()
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var info windows.ProcessInformation
	if err := windows.CreateProcess(app, command, nil, nil, true, flags, &block[0], dir, &startup.StartupInfo, &info); err != nil {
		return nil, fmt.Errorf("create job-owned process: %w", err)
	}
	runtime.KeepAlive(cfg.Files)
	runtime.KeepAlive(block)
	defer windows.CloseHandle(info.Process)
	defer windows.CloseHandle(info.Thread)
	// The child already belongs to the job even if its owner dies inside this
	// callback, before FindProcess/ResumeThread or any caller can observe Start.
	if afterCreate != nil {
		afterCreate(info.ProcessId)
	}
	process, err := os.FindProcess(int(info.ProcessId))
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		var count uint32
		count, err = windows.ResumeThread(info.Thread)
		if err == nil && count != 1 {
			err = fmt.Errorf("unexpected initial thread suspend count: %d", count)
		}
	}
	if err != nil {
		_ = windows.TerminateProcess(info.Process, 1)
		if process != nil {
			_, _ = process.Wait()
		} else {
			_, _ = windows.WaitForSingleObject(info.Process, windows.INFINITE)
		}
		return nil, err
	}
	return process, nil
}
