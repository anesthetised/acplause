# ADR 0025: Bound an opt-in shell sandbox on macOS

- Status: accepted
- Date: 2026-10-22
- Issue: [#131](https://github.com/anesthetised/acplause/issues/131)
- Related: [ADR 0003](0003-permissions.md), [ADR 0007](0007-mcp-servers.md),
  [ADR 0013](0013-plan-build-mode.md), [ADR 0014](0014-stream-agent-terminals.md),
  [ADR 0022](0022-provider-key-exposure.md), [ADR 0024](0024-release-distribution.md)

## Context

`os.Root` confines built-in file operations. An approved `bash` invocation
currently runs with the user's privileges, inherited environment minus
`ACPLAUSE_API_KEY`, and the session working directory. Process-group cleanup
handles ordinary descendants; it is not a boundary against a command that
deliberately changes its process group. Unsetting the provider key prevents
accidental inheritance, not inspection of the parent by a same-user process.

The user selected **macOS with local developer tools first, Linux later**.
The work remains a design for the existing foreground shell tool. Background
jobs ([#119](https://github.com/anesthetised/acplause/issues/119)) and interactive
stdin/PTY support
([#123](https://github.com/anesthetised/acplause/issues/123)) remain parked.

The smallest native macOS route is `/usr/bin/sandbox-exec`, applying a Seatbelt
policy before executing bash. Its presence does not establish a supported
Apple API: the command is deprecated and Apple DTS says the policy language
has never been a supported third-party interface [1]. Choosing it means
accepting an explicit compatibility and maintenance risk.

## Threat model

Treat the command, repository content, build scripts and all shell descendants
as potentially hostile even after ACP permission. The intended boundary is
between that invocation and resources outside its effective policy.

| Asset or action | Intended restriction |
| --- | --- |
| Files outside granted paths | Deny content reads and writes; identify any necessary metadata exceptions separately. |
| Workspace and additional directories | Grant explicitly as working data; files and secrets inside them are exposed to the command. |
| Git configuration and hooks in granted roots | Deny writes, so a confined command cannot plant code that later Git commands run outside the sandbox. |
| Toolchains and SDKs | Explicit read/execute grants; no automatic grant to the whole home directory. |
| Temporary files and caches | Private invocation scratch plus explicitly granted cache paths; no blanket shared-temp write grant. |
| Network and host IPC | Deny external and loopback TCP/UDP, Unix sockets and authority-bearing host services unless the fixed runtime policy demonstrates a necessary safe exception. |
| Agent and unrelated processes | Deny inspection/debugging and signals that could read secrets or control those processes. |
| Descendants | Restrictions must survive fork/exec and detachment; containment and cleanup are separate properties. |

The trusted computing base includes the OS, acplause, its launch configuration,
the sandbox launcher, and the fixed policy. The ACP client controls permissions
and session roots. A malicious client, compromised kernel, hostile administrator
or an independent attacker already controlling the user's account is outside
this boundary. No CPU, memory or disk quota is promised by this proposal.

This is **shell confinement**, not isolation of the whole agent. Built-in reads
can still return allowed workspace content to the model API. MCP remains an
independent authority with its existing per-call permission policy. Secrets
intentionally supplied in shell environment values, permitted files, command
arguments or tool output remain exposed. An allowed writable cache can carry
modified artifacts into a later build; it is not a trusted artifact store.
The same holds for the writable workspace itself: build files, `.envrc`,
editor task definitions and `AGENTS.md` (which acplause reads into the system
prompt every round) can be changed by a confined command and later run or
read without confinement. Only Git configuration and hooks are protected,
because clients and users run Git in the workspace routinely and without a
further prompt; other workspace content stays a documented residual risk.
Grants cover reachable objects, not independent copies: a pre-existing hardlink
inside a granted root exposes its outside alias to reads, and inside a writable
root to modification as well. Do not promise snapshot isolation or protection
of aliases to granted data.

## Decisions

The user accepted this design on 2026-10-22, including the unsupported-profile
risk, offline scope, separate prototype task and protection of Git
configuration and hooks. Acceptance does not establish that its security
properties have been implemented or tested, or authorize production
integration before the prototype gate.

1. **An experimental, opt-in Seatbelt backend for foreground bash only.**
   Keep sandboxing off by default. Invoke the absolute system launcher with
   an agent-owned policy and separately supplied path parameters; do not
   interpolate paths or commands into policy source. Session and configured
   roots vary in number, so the policy may be generated per invocation, but
   only from indexed parameter names (`ROOT_0`, `ROOT_1`, …) under a fixed
   upper bound; paths reach the launcher only as `-D` values, and a request
   beyond the bound fails instead of merging roots. Launch bash only after
   restrictions apply. Resolve its executable from trusted launch configuration,
   outside shell-writable paths. No new Go module, cgo helper, Node runtime,
   daemon or custom signing requirement is proposed. The deprecated system
   executable is still a platform dependency.

2. **Start with an offline resource policy.** Deny by default. Grant session
   `cwd` and additional directories, reviewed runtime/toolchain reads, and
   private scratch. Permit extra read-only or writable paths only from trusted
   launcher configuration. Deny shell network access, including host loopback,
   and access to SSH agents, Docker sockets, Launch Services/Apple Events and
   other host services that can act outside the sandbox. No network proxy,
   domain filter or per-command escape hatch in the first slice. Dependency
   downloads, remote git operations and network-dependent tests (including
   tests that create localhost servers) fail; users
   must provision dependencies outside this shell policy. Local tools and
   project-built executables remain a required positive test, not an assumption.

   Within writable roots, deny writes to Git configuration and hooks:
   `config`, `config.worktree` and `hooks/` of any `.git` directory, including
   its `worktrees/` and `modules/` subdirectories. Ordinary local Git work
   (status, add, commit, branch, checkout) stays possible; changing repository
   configuration or hooks must happen outside this shell. A path-based rule is
   insufficient if renaming a `.git` directory or one of its ancestors, or
   replacing it, lets the command write those files under another name; the
   prototype must show these routes are closed or report the gap.

3. **One immutable launch policy, with session roots bound per invocation.**
   The configuration contract is `ACPLAUSE_BASH_SANDBOX=off|seatbelt` and optional
   `ACPLAUSE_BASH_SANDBOX_CONFIG`, an absolute path to a bounded JSON file with
   `version: 1`, `readOnlyPaths` and `writablePaths` arrays of absolute paths.
   These grant filesystem access only; they cannot enable networking or IPC.
   Missing config means no user-supplied extra paths. Reject unknown modes,
   malformed policy and config supplied while the backend is off. Do not
   discover policy in a repository or let model arguments weaken it. Read it
   once at process startup; changes require a restart.

   Normalize aliases and resolve symlinks before describing the policy. Keep
   directory identities as well as display paths; root replacement between
   permission and launch must fail, and adversarial rename/symlink tests must
   show that acquisition cannot reach an ungranted resource. A check followed
   by an unchecked pathname operation is not a proof of race safety. Freeze
   roots and policy with the prepared call so resume/reconfiguration cannot
   redirect an already approved invocation. Reject grants that expose the
   agent's data directory, policy file or trusted launcher/executable to shell
   writes, and reject a workspace containing those protected resources. Do not
   resolve a conflict by silently widening the policy.

4. **Preserve credentials and make temporary access explicit.** Keep
   `ACPLAUSE_API_KEY` removed as in ADR 0022. Retain the remaining shell
   environment, except set `TMPDIR` to private invocation scratch when
   sandboxing is enabled. Do not rewrite `HOME` or infer grants from `PATH`,
   cache variables, proxy variables or `SSH_AUTH_SOCK`. A variable naming an
   inaccessible location grants no access. Configure needed caches through
   explicit paths or tool-specific variables; do not automatically expose
   shared caches or user configuration to make a failing tool work.
   The `TMPDIR` override is an explicit, opt-in exception to ADR 0022's
   otherwise-unchanged environment rule. No change applies to MCP processes.
   Pass only intended standard streams and documented launch handles; close
   policy handles before the payload. Seatbelt cannot revoke access through
   a descriptor already open when confinement begins [2].
   Audit launcher initialization under loader-affecting environment variables:
   workspace-controlled code must not run before confinement. Preserving the
   payload environment is not permission to inject code into an unrestricted
   launcher. If the system launcher cannot provide that boundary, stop at the
   prototype gate and revise this design before adding a helper or changing
   environment semantics further.

5. **Keep permission distinct from confinement.** Every bash call still needs
   `allow_once` under ADR 0003, and Plan mode still rejects bash. Describe the
   experimental backend, network denial and effective filesystem grants before
   permission using existing ACP text/tool content; preserve the actual command
   and cwd in the permission subject. Do not invent wire fields. Approval
   authorizes that prepared command under that policy; a denied resource never
   triggers a second execution with broader rights. Expose configured mode and
   capability results in `doctor`, with secret-free errors. Logs stay on stderr.

6. **Fail when requested protection is unavailable.** Before serving with
   `seatbelt`, validate configuration and execute bounded local self-checks
   using disposable fixtures and an embedded, non-model payload. Check both
   permitted access and denied access: executable presence or successful
   `true` is insufficient. An unsupported OS/architecture, missing launcher,
   policy compilation failure or failed required check prevents startup.
   Session-specific roots are validated during setup, outside the ordered
   JSON-RPC reader. Failure preserves the old session configuration.
   A failure detected after a model tool call becomes a failed tool result;
   never silently retry unsandboxed. A startup probe is a compatibility check,
   not a security proof; support also requires the release test matrix below.

7. **Preserve lifecycle and bounded output; prove its limits.** Keep timeouts,
   turn cancellation, session close/ownership loss and terminal finalization
   ordered as today. Do not equate group `SIGKILL` with descendant containment.
   Test detached/orphaned descendants for continued confinement and separately
   for termination on timeout, cancellation and normal shell exit. Production
   integration is blocked if the chosen launcher cannot meet the existing
   foreground lifecycle contract; do not add a background-job manager or
   silently weaken that contract under this ADR. Retain scratch until owned
   execution has finished and clean up only the invocation-owned directory.

## Alternatives considered

| Option | Benefit | Cost or limitation |
| --- | --- | --- |
| Experimental native Seatbelt, offline first | Small launch adapter, native tools, no added package runtime | Deprecated/unsupported profile interface; acplause owns policy validation and OS regressions; offline workflows only initially. |
| External Anthropic sandbox-runtime | Reuses filesystem and network-proxy policy for native tools | Research-preview dependency; Node and platform tools; still Seatbelt on macOS; adapter must enforce failure rather than accept degraded protection [3]. |
| Apple App Sandbox helper | Supported entitlement-based mechanism | Static executable/resource access is awkward for arbitrary SDKs, Homebrew and generated programs; signed-helper packaging and scoped access need a separate design [4]. |
| Endpoint Security descendant client | Supported descendant-scoped API on macOS 27 | Managed Apple entitlement, distribution changes and a policy implementation; not a ready-made filesystem/network sandbox [5]. |
| External Linux container or VM | Can separate host files/processes using an existing runtime | Different toolchain on Mac, image/runtime and path-mount lifecycle; moving the whole agent also changes MCP/API reachability. |
| No native backend yet | Avoids an unverified platform security contract | Does not meet the selected local-macOS scenario; current shell remains permission-gated without OS isolation. |

We choose the **experimental native, offline design and its validation gate**,
then decide whether measured compatibility justifies runtime integration.
If network access becomes required in the first slice, reconsider the
external-runtime option instead of adding a custom proxy to this bridge.

For the external-container alternative, mounts are authority: Docker bind mounts
are writable by default, and exposing the daemon can give the command control
over host resources [6]. Rootless mode reduces daemon privilege but requires
host setup [7]. Putting acplause and bash together inside one container does
not itself isolate bash from the agent's key or session data. Giving the whole
container no network also prevents acplause reaching its HTTP model API [8].

## Platform matrix

| Platform | Current runtime | Proposed first slice | Follow-up |
| --- | --- | --- | --- |
| macOS arm64/amd64 | No shell isolation | Experimental Seatbelt only after real isolation and toolchain gates pass on explicitly recorded OS releases | Revalidate supported OS releases/architectures; fail requested mode on unvalidated combinations. |
| Linux arm64/amd64 | No shell isolation | No backend; requested `seatbelt` fails | Separate decision on namespaces/seccomp through bubblewrap versus Landlock; dependency approval required. |
| Native Windows | No shell isolation; non-Unix cleanup kills only the direct child | No backend; requested `seatbelt` fails | No implementation commitment. AppContainer is a different token/capability/ACL integration [11]. |
| MCP, any platform | Existing client-configured execution and permissions | No change from this shell-only feature | Separate explicit scope decision. |

Linux needs runtime feature detection, not just a kernel version comparison.
Landlock restrictions vary by ABI; current kernel documentation describes TCP
from ABI 4, UDP from ABI 10, pathname Unix-socket control from ABI 9 and signal/
abstract-socket scoping from ABI 6 [9]. A proposal must require the features it
promises rather than silently dropping missing restrictions. Bubblewrap supplies
namespaces and optional seccomp, but its caller still owns the security policy
and must account for host sockets and the host's user-namespace policy [10].
The Linux research favors bubblewrap plus an explicit socket/seccomp policy
over Landlock alone: Landlock does not restrict all filesystem metadata
changes, and a separate PID namespace offers a stronger cleanup primitive.
A network namespace alone still exposes pathname Unix sockets in mounted
directories. These are reasons for a separate bounded Linux design, rather
than assuming backend equivalence.
Neither candidate is selected by this macOS-first ADR.

## Verification and follow-up slices

No sandbox probe, installation or benchmark was run for this document. Research
and local code/manual inspection establish candidates, not working protection.

1. **Disposable policy validation, after design approval.** Build a test-only
   fixture executable and fixed profile, separate from the production shell
   path. Test allowed operations and denied same-user-readable sibling files,
   outside writes, symlink/rename escapes, path-prefix collisions and macOS
   `/var` aliases. Use random fake secrets in dummy processes; test process
   inspection/debugging and harmless signals without targeting user processes.
   Exercise TCP/UDP over IPv4/IPv6, loopback, Unix sockets in allowed directories
   and fixture host services. Confirm test endpoints are reachable without the
   sandbox so unrelated failures cannot masquerade as protection. Test inherited
   handles, loader-affecting environment, hardlink limitations (including
   whether a confined command can create a new hardlink to an ungranted file)
   and fork/exec, grandchildren, `setsid` and orphaning explicitly. Test denied
   writes to Git configuration and hooks directly, through worktree and
   submodule Git directories, and after renaming or replacing `.git` or an
   ancestor.

2. **Compatibility and lifecycle gate.** Record OS build, architecture, policy
   revision, toolchain and effective paths. Demonstrate local git operations,
   offline Go build/test, SDK access and execution of a just-built fixture.
   Test cached tools with explicit grants and demonstrate denial of ungranted
   caches. Record tools that find their temporary directory other than through
   `TMPDIR` (for example the per-user Darwin temporary directory) and whether
   they fail under the private scratch policy. Prove
   timeout/cancel/close/ownership-loss cleanup and terminal output ordering,
   including detached descendants. Any security or lifecycle failure
   blocks integration; do not silently broaden the policy. Present findings
   before authorizing a production backend.

3. **Separate runtime integration.** Add configuration, doctor/self-checks,
   policy visibility and the narrow launch adapter only after the gate passes.
   Add in-package tests plus schema-validated e2e tests for failed setup,
   refused/allowed permission, runtime denials, immutable prepared calls,
   cancellation and complete model tool-call results. Preserve coverage >=80%.
   Add macOS CI for the claimed platform matrix; today's Ubuntu-only CI cannot
   establish a macOS security guarantee. An unavailable required runner remains
   an unmet support gate, not a passing skip. Run `just check` and actual T3
   provider smoke tests before claiming runtime support.

4. **Measure practical overhead before shipping.** First agree workload and
   resource budget with the user. Suggested measurements are command launch
   latency and representative offline builds/tests, comparing off/on under
   the same cache and scratch conditions. Record startup probes separately.
   No benchmark or optimization claim is part of this design change.

Track the prototype in [#133](https://github.com/anesthetised/acplause/issues/133),
separately from production integration. Keep #131 open
until this accepted ADR lands in the repository; acceptance never implies that
any platform row has passed runtime verification.

The [2026-10-22 prototype record](../testing/seatbelt-prototype.md) recommends
**no production integration**: detached descendants survive foreground cleanup,
local Xcode tool workflows fail, and required security checks remain unverified.
The accepted contract and its production gate remain unchanged.

## Consequences

The accepted experimental direction gives acplause a bounded next step
without adding a general execution platform. It also makes policy maintenance,
macOS testing and denied-tool diagnostics project responsibilities. Some
ordinary developer workflows will need explicit paths or remain unavailable.
Native support must be withdrawn or refused if an OS update invalidates its
required behavior. A maintained third-party runtime remains a viable alternative
if these costs outweigh a small direct adapter.

## Sources

Reviewed 2026-10-22. Platform/project documentation describes mechanisms;
acplause-specific restrictions and gates above are proposed design choices.

1. [Apple DTS: custom sandbox support and current alternatives](https://developer.apple.com/forums/thread/777509).
2. [Chromium: macOS sandbox and pre-opened descriptors](https://chromium.googlesource.com/chromium/src/+/main/sandbox/mac/README.md),
   [policy controls](https://chromium.googlesource.com/chromium/src/+/main/sandbox/policy/mac/common.sb).
3. [Anthropic sandbox-runtime](https://github.com/anthropics/sandbox-runtime),
   [package runtime requirements](https://github.com/anthropics/sandbox-runtime/blob/main/package.json).
4. [Apple: embedded sandboxed tools](https://developer.apple.com/documentation/xcode/embedding-a-helper-tool-in-a-sandboxed-app),
   [DTS: executable access and inherited restrictions](https://developer.apple.com/forums/thread/807142).
5. [Apple: descendant Endpoint Security client](https://developer.apple.com/documentation/endpointsecurity/es_new_descendants_client(_:_:)),
   [required entitlement](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.endpoint-security.client).
6. [Docker: bind mounts](https://docs.docker.com/engine/storage/bind-mounts/),
   [daemon authority](https://docs.docker.com/engine/security/).
7. [Docker: rootless requirements](https://docs.docker.com/engine/security/rootless/).
8. [Docker: network none](https://docs.docker.com/engine/network/drivers/none/).
9. [Linux kernel: Landlock ABI and restrictions](https://docs.kernel.org/userspace-api/landlock.html).
10. [Bubblewrap: policy responsibility and limits](https://github.com/containers/bubblewrap).
11. [Microsoft: AppContainer capabilities and launch](https://learn.microsoft.com/en-us/windows/win32/secauthz/implementing-an-appcontainer).
