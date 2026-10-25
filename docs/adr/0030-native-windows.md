# ADR 0030: Native Windows support

- Status: accepted
- Date: 2026-10-25
- Issue: [#178](https://github.com/anesthetised/acplause/issues/178)
- Related: [ADR 0024](0024-release-distribution.md),
  [ADR 0025](0025-shell-sandbox.md),
  [ADR 0026](0026-shell-lifecycle-after-seatbelt.md)

## Context

The program cross-compiles for Windows, but shell execution assumes bash,
non-Unix cleanup kills only the direct process, tests assume Unix paths and
commands, and distribution excludes Windows. A successful cross-build alone
does not establish usable Windows support.

## Decision

The maintainer selected native Windows with PowerShell, Job Objects, native
CI and ZIP releases on 2026-10-25, including direct use of the already pinned
`golang.org/x/sys` dependency. Support Windows 10/11 on amd64 and arm64 within
the Go toolchain's supported OS baseline [1]. Keep release binaries CGO-free.

- On Windows advertise `powershell`, using Windows PowerShell without profiles
  or interactive prompts. Pass a UTF-16LE Base64 payload decoded into a script
  block under `-Command` [2, 6] and select UTF-8 for console output.
  PowerShell 5.1's `-EncodedCommand` serializes stderr as CLIXML even with text
  output selected; the ASCII decoding launcher preserves quoting and Unicode
  while keeping errors textual. Unix continues to advertise `bash`.
  Keep the command argument, permission subject, remembered decisions, terminal
  updates and existing `ACPLAUSE_BASH_TIMEOUT` setting for both platforms.
- Place each shell invocation and stdio MCP server in a private, unnamed Job
  Object with kill-on-close and no breakaway flags [3]. Use `CreateProcessW`
  with `PROC_THREAD_ATTRIBUTE_JOB_LIST` so membership is atomic with creation
  [5, 7]. Create suspended, then resume the returned initial thread handle;
  reject launch if setup fails. Cancellation and timeout terminate the shell job; normal shell exit
  also cleans up its remaining members. MCP keeps its existing stdin-close
  grace period before terminating its job. The job handle is not inherited;
  agent death closes it. This uses documented Windows APIs through x/sys;
  no helper executable, CGO, PID-based tree-kill command or external runtime.
- Keep file operations behind `os.Root`. Validate Windows drive paths and
  separator handling; do not interpret ACP paths as URLs or change wire shapes.
- Add native Windows unit/e2e CI, preserve Unix-specific tests under build tags,
  and cross-build arm64. Portable behavior retains its assertions on Windows.
  Existing Linux CI remains responsible for the >=80% coverage gate.
- Add amd64/arm64 Windows ZIP archives to checksums, attestations and saved
  release bundles without weakening immutable release retries (ADR 0024).

## Alternatives considered

- **Require Git Bash:** smaller shell change, but requires an extra runtime and
  retains Unix shell semantics for native Windows users. Not selected.
- **WSL2 only:** reuses the Linux build but does not provide a native Windows
  ACP process. Not selected.
- **PowerShell 7 only:** provides newer semantics but requires installing pwsh.
  Use the Windows-provided PowerShell baseline instead.

## Consequences and limits

The post-merge review identified an owner-death gap between `exec.Cmd.Start`
and `AssignProcessToJobObject`: a suspended child could survive outside the
job. [#180](https://github.com/anesthetised/acplause/issues/180) replaces that
sequence with atomic creation. Go's `os/exec` does not expose the job-list
attribute, so Windows uses a small x/sys launcher with an explicit stdio handle
allowlist and standard `os.Process.Wait`; Unix keeps `os/exec`. Tests stop the
owner immediately after native creation, before resuming the child.

Windows commands use PowerShell syntax, not translated bash scripts. Tool
descriptions must state the actual shell. PowerShell 5.1 file redirection has
its own encoding semantics; UTF-8 console output does not change every cmdlet's
file encoding [4]. Batch-file MCP entry points should be launched explicitly
through their command interpreter; acplause does not implicitly shell-expand
MCP configuration.

Job Objects provide process membership and teardown, not filesystem/network
confinement or a security boundary against commands running as the same user.
Work started indirectly through external brokers/services may not belong to
the job [3]. This does not enable the deferred sandbox backend or relax its
requirements. Report cleanup failures rather than silently claiming success.

Native CI establishes automated Windows behavior. Actual T3 Windows-provider
interoperability still requires a separate real-client runtime smoke record;
neither cross-builds nor scripted ACP tests establish it. arm64 cross-builds
must be distinguished from native arm64 test execution.

## Verification

Run `just check`, total statement coverage >=80%, Windows amd64/arm64 builds,
and native Windows CI. Include shell Unicode/quoting/exit status, streaming and
cancellation, descendant cleanup on shell exit and agent death, MCP teardown,
workspace escape rejection, Windows SQLite persistence, and release bundle
tests for missing/tampered ZIP archives. Preserve existing Unix behavior.

## Sources

1. [Go: minimum requirements](https://go.dev/wiki/MinimumRequirements).
2. [Microsoft: powershell.exe](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_powershell_exe?view=powershell-5.1).
3. [Microsoft: Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects).
4. [Microsoft: character encoding](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_character_encoding?view=powershell-5.1).
5. [Microsoft: CreateProcessW](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessw),
   [process creation flags](https://learn.microsoft.com/en-us/windows/win32/procthread/process-creation-flags),
   and [ResumeThread](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-resumethread).
6. [Microsoft: ScriptBlock.Create](https://learn.microsoft.com/en-us/dotnet/api/system.management.automation.scriptblock.create).
7. [Microsoft: process attributes and JOB_LIST](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute).
