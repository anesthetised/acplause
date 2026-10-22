# macOS Seatbelt prototype: no production integration

Date: 2026-10-22. Issue: [#133](https://github.com/anesthetised/acplause/issues/133).
Contract: [ADR 0025](../adr/0025-shell-sandbox.md).
Source baseline: `6f22940`; branch: `test/133-seatbelt-prototype`.
Fixture commit: `634b9f9`.

**Outcome: no-go for production integration.** The real bash toolbox
returns and finalizes its output while a confined descendant that used
`setsid`, double fork and exec continues running. This reproduces on normal
exit, timeout and cancellation. ADR 0025 decision 7 makes that a blocking
lifecycle failure. The profile also fails the local Xcode tool workflows,
and the debugging and host-IPC requirements are not established.

The experiment does not activate a runtime backend, change ACP, add a dependency,
modify MCP, install software or change host security/licensing settings. It
uses disposable files, local echo listeners and dummy processes with random
synthetic environment markers. No benchmark was run. This result supports
neither an additional platform nor a shipping/support claim.

## Reproduce

On macOS with the already installed Go and Xcode/Command Line Tools:

```sh
just seatbelt-prototype
```

This invokes:

```sh
go test -race -count=1 -tags seatbelt_prototype -v ./experiments/seatbelt/
```

The full experiment **fails** at unmet gates. They remain assertions, rather
than expected successes or skips. `taskport` reports an unavailable positive
control as a failed, unverified gate. Use `-run` to isolate a probe, for example
`just seatbelt-prototype -run Lifecycle`.

[The Go harness](../../experiments/seatbelt/prototype_test.go) compiles the
[native fixture](../../experiments/seatbelt/testdata/probe.c) using the absolute
`/usr/bin/clang`. The fixture is not a cgo or production helper. The build tag
keeps this host-dependent, deliberately failing experiment out of the normal
test suite; a non-macOS host cannot establish these gates. Ordinary CI remains
Ubuntu-only and supplies no macOS support evidence.

Each lab owns a fresh `/private/tmp/acplause-seatbelt-*` directory. Test payloads
receive a synthetic environment, with no provider key or inherited credentials.
The harness closes listeners, waits for their workers and removes its fixtures.
Detached dummy workers close standard streams, obey an invocation-owned lease
file and self-expire after five seconds. Cleanup removes that lease and waits
for their completion marker. No cleanup target is taken from a process listing
or a PID file.

## Recorded environment and policy

- macOS **27.0.1**, build **26A434**, **arm64**.
- Go **1.27.3**; `GOROOT=/opt/homebrew/Cellar/go/1.27.3/libexec`.
- Apple Git **2.54.0 (Apple Git-157)**.
- Apple clang **21.0.0 (clang-2100.3.34.2)**; Xcode developer directory
  `/Applications/Xcode.app/Contents/Developer`.
- Launcher: absolute `/usr/bin/sandbox-exec`.
- [Fixed profile](../../experiments/seatbelt/testdata/offline.sb), revision **2**,
  SHA256 `9165a3a9a57417be36f36181ad423f1bbb4bc2541a6c8fd74bb15dea2e2adc10`.

The launcher receives four writable roots: workspace, an additional directory,
a private cache and invocation scratch. Four read-only roots supply the fixture
executable, read-only test data, the exact Go installation and Xcode's
`/Applications/Xcode.app/Contents` tree. The Xcode grant includes adjacent
frameworks loaded by developer tools. All varying paths are separate indexed
`-D` arguments; no path or command is inserted into profile source.

Fixed runtime reads cover `/bin`, `/sbin`, `/usr/bin`, `/usr/sbin`, `/usr/lib`
and `/System/Library`. `/dev/null` permits reads/writes and random devices
permit reads. Explicit exceptions are global file metadata reads, reading the
root directory itself, and the finite CPU/page-size/OS sysctl names in the
profile. Opening `/` was necessary for this OS's dyld initialization of a
locally compiled C executable: without it the positive fixture aborted before
performing an operation. This permits root-directory enumeration, not arbitrary
file contents. `hw.pagesize_compat` was needed by the Go runtime on this host.

There is no general home, shared-temp, network, Unix-socket or Mach-service
grant. `HOME` is unchanged; `TMPDIR` names private scratch. Go uses explicit
private `GOCACHE`/`GOMODCACHE`, `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`,
`GOSUMDB=off` and `CGO_ENABLED=0`. The Go fixture has no non-stdlib dependencies.
These test conditions do not establish compatibility with an arbitrary user's
unchanged shell environment.

## Observations

### Filesystem and Git protection

`TestSeatbeltFilesystem`, `Aliases`, `GitProtection` and `RootReplacement`:

- Workspace, additional-root, scratch and explicit-cache access succeeds.
  Read-only fixture reads succeed and writes fail with `EPERM`.
- Same-user-readable sibling contents, outside writes/chmod, path-prefix
  collisions and symlinks to outside data fail with `EPERM`. `/tmp` and
  `/private/tmp` aliases give the same allowed/denied results. Outside `stat`
  succeeds under the declared metadata exception.
- Creating a new hardlink from an ungranted outside file fails. A hardlink
  planted inside a writable root **before** confinement remains writable and
  changes its outside alias. This is ADR 0025's explicit reachable-object
  limitation, not isolation of separate copies.
- Direct `config`, `config.worktree` and hook writes fail under ordinary `.git`,
  worktree and nested submodule administration paths. Replacing a config by
  rename and relocating `.git`, its parent repository, a submodule gitdir or a
  worktree gitdir does not enable the tested write under revision 2. These
  rename/write operations succeed on equivalent unconfined fixtures.
- Revision 1 blocked direct config/hook writes but allowed moving
  `.git/modules/mod` or `.git/worktrees/linked` out of `.git` and then modifying
  their configs. Revision 2 additionally denies unlink/rename of directories
  within those administration trees. It consequently forbids directory
  removal there, including some worktree-removal/GC activity; compatibility of
  ordinary submodule/worktree operations is still unverified.
- Deterministically replacing the captured workspace path with an outside
  symlink before launch, and after a running confined fixture reaches a
  barrier, produces `EPERM` when reading through that path. This tests those
  two substitutions only. It does not prove atomic root acquisition, validate
  a production permission-to-launch identity check or cover adversarial races.

### Network, inspection and inherited authority

`TestSeatbeltNetwork`, `ProcessAccess`, `InheritedDescriptor` and `Loader`:

- TCP and UDP echo exchanges over IPv4/IPv6 loopback succeed unconfined and
  fail confined with `EPERM`. A pathname Unix echo socket **inside the granted
  writable workspace** behaves the same way. No remote endpoint was contacted.
- `proc_pidinfo`, `KERN_PROCARGS2` and harmless `SIGUSR1` access to a dummy
  sibling process succeed unconfined and fail confined. The unconfined
  `KERN_PROCARGS2` control verifies that its buffer contains the random fake
  environment marker without printing that marker or any environment bytes.
- Revision 1's broad `sysctl-read` grant disclosed the dummy process's argument/
  environment buffer despite denying `process-info*`. Revision 2 replaces it
  with the finite, named runtime queries. A blanket sysctl grant is unsafe.
- `task_for_pid` returns **5** (`KERN_FAILURE`) even unconfined. Debugger denial
  attributable to this profile is **unverified**, and its gate fails.
- A deliberately inherited descriptor for an outside file remains readable
  under confinement. Normal harness launches pass only standard streams;
  the descriptor probe intentionally passes one extra handle. A real launcher
  still needs its own descriptor/initialization audit.
- A test dylib constructor can write an outside canary when loaded directly
  into the unconfined fixture. Through system `sandbox-exec`,
  `DYLD_INSERT_LIBRARIES` is stripped from the payload environment and the
  canary is absent. This is one observed loader control, not proof for every
  loader-affecting variable or an unchanged payload environment contract.
- A workspace-controlled `BASH_ENV` script executes, but its attempt to write
  the outside canary is denied. The same script succeeds unconfined.

### Confinement and foreground lifecycle

`TestSeatbeltLifecycle` runs the current exported `tools.Toolbox` bash path and
its real `ShellObserver`, with the sandbox launcher as the trusted test command.
The probe compares ordinary descendants with a closed-stdio grandchild that
calls `setsid`, double-forks and execs the fixture again.

All tested descendants retain the outside-read restriction. Ordinary
process-group members stop on exit, timeout and cancellation. In each of the
three detached cases the observer has finalized before the toolbox returns,
but a heartbeat file continues growing afterward:

```text
detached-exit:    confinement=denied; output-finalized=true; descendant-active-after-return=true
detached-timeout: confinement=denied; output-finalized=true; descendant-active-after-return=true
detached-cancel:  confinement=denied; output-finalized=true; descendant-active-after-return=true
```

The lease and self-expiration terminate these test workers after observing the
failure; they are test cleanup, not a proposed runtime process manager. Kernel
confinement survives detachment, while process-group cleanup does not. No
production adapter is justified by these results.

### Local-tool compatibility

`TestSeatbeltLocalTools` uses independent workspaces and unconfined controls:

- Local Git status/add/commit/branch/checkout succeeds unconfined. Under the
  profile, Apple's Git shim reports an unaccepted Xcode license and exits 69.
- Clang compiles and executes a stdio-using C fixture unconfined. The same
  compilation under the profile reports the Xcode license error, exit 69.
  No license agreement or system setting was changed. The exact missing
  licensing/configuration authority is not established by this experiment.
- Pure-Go offline build, execution of the generated binary and a test run
  succeed in both modes. Build uses `-a` and tests use `-count=1`, so a cached
  test result does not stand in for confined execution. This does not establish
  cgo, external-module or acplause-build compatibility.
- `confstr(_CS_DARWIN_USER_TEMP_DIR)` differs from `TMPDIR` unconfined, but
  returns private `TMPDIR` inside this profile on this OS. No blanket shared
  temporary-directory grant was added. Other tools' temporary-directory
  discovery remains unverified.
- The tested localhost services are intentionally inaccessible. This policy
  cannot run the project's network-dependent test suite as-is.

## Unmet gates and maintainer decision

The lifecycle failure alone blocks production integration. Do not add the
configuration variables, doctor activation or a launch adapter under the
assumption that a stricter filesystem profile repairs descendant ownership.
On 2026-10-22 the maintainer accepted deferring native sandbox integration while
preserving the existing requirements and this no-go result, recorded in
[ADR 0026](../adr/0026-shell-lifecycle-after-seatbelt.md). Any different cleanup
mechanism, helper, backend, environment contract or relaxed requirement still
needs a separate design decision. No replacement was selected.

The following requirements are also **unverified**: authority-bearing Mach/
host-IPC fixture services; debugger access with a working positive control;
dummy-parent inspection; parent-death cleanup; session-close/ownership-loss
tests through an activated backend; adversarial acquisition/rename races;
alternate Git administration layouts and case aliases; protected agent-data/
policy/launcher-root rejection; complete launch-handle/loader-variable audit;
other OS builds, macOS amd64 and macOS CI. The experiment stops at the
demonstrated no-go rather than treating these unrun gates as passing.

## Repository validation

- `just check -coverprofile=cover.out`: passed, including the schema-validated
  e2e suite, race detector, vet, Staticcheck, release checks and module-tidy diff.
- `go tool cover -func=cover.out | tail -1`: **92.2%** total Go statements.
- `go vet -tags seatbelt_prototype ./experiments/seatbelt` and
  `staticcheck -tags seatbelt_prototype ./experiments/seatbelt`: passed.
- Temporarily changing the config-protection rule from deny to allow makes
  the `.git/config` probe fail on the actual successful write. The exact
  profile was restored afterward. A payload abort also fails the harness;
  it is not counted as an access denial.
- `just seatbelt-prototype`: fails at the three detached-lifecycle cases,
  the unverified task-port control and the Git/SDK compatibility cases.

Apple DTS describes custom SBPL as unsupported and its public entry points as
deprecated ([Apple discussion](https://developer.apple.com/forums/thread/777509)).
Chromium documents inherited-descriptor authority and default-deny policies
([Mac sandbox](https://chromium.googlesource.com/chromium/src/+/main/sandbox/mac/README.md)).
The checked-in profile is acplause experiment code, not a security guarantee
derived from those sources.
