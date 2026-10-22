# ADR 0026: Reassess native shell confinement after the Seatbelt no-go

- Status: proposed; awaiting maintainer agreement
- Date: 2026-10-22
- Issue: [#133](https://github.com/anesthetised/acplause/issues/133)
- Related: [ADR 0014](0014-stream-agent-terminals.md),
  [ADR 0024](0024-release-distribution.md),
  [ADR 0025](0025-shell-sandbox.md)

## Context

The [prototype report](../testing/seatbelt-prototype.md) establishes a no-go
for ADR 0025's production integration gate. A descendant can retain Seatbelt
restrictions while surviving normal shell exit, timeout and cancellation after
`setsid`, double fork and exec. Closed standard streams also let the toolbox
finish its output while the worker continues. The failure concerns ownership
of execution, independently of filesystem confinement.

Current cleanup sends `SIGKILL` to a process group. It neither proves that all
descendants stayed in that group nor waits for each descendant to terminate.
More filesystem rules cannot repair those properties. Toolchain compatibility,
debugging, host IPC and the other unverified gates remain separate blockers.

ADR 0025 requires native macOS tools, complete foreground cleanup and no added
helper/runtime/signing requirement. ADR 0024 distributes CGO-free binaries and
supports local source builds without a paid Apple signing certificate. The
mechanisms reviewed below do not establish a solution satisfying all of these
constraints. This is a bounded review, not a proof that no macOS solution exists.

## Proposed decision

**Recommend deferring native sandbox integration while preserving the lifecycle
requirement and current distribution contract (option A).** Retain the failing
probes and no-go report. Describe the existing process-group limit accurately
in user documentation and source comments.

Before another implementation attempt, obtain agreement on a mechanism with
an explicit owner for all execution and the changes needed to distribute it.
Do not add a PID scanner, background-job manager or fallback to broader rights
to make the current gate appear to pass. Fixing the Xcode compatibility cases
alone would not change this recommendation.

This proposal is not accepted. ADR 0025 remains in force, including its blocked
integration gate. No helper, dependency, platform change or weaker lifecycle
guarantee is authorized by this document.

## Mechanisms reviewed

- **Process groups and launchd.** The native fixture demonstrates escape from
  group cleanup. The installed macOS 27.0.1 `launchd.plist(5)` entry for
  `AbandonProcessGroup` likewise describes killing remaining members of the
  job's group. Apple's launchd guide tells cooperating daemons not to call
  `setsid` [1]; that advice does not constrain hostile payloads.
- **kqueue and process discovery.** Apple's public XNU header marks
  `NOTE_TRACK`, `NOTE_TRACKERR` and `NOTE_CHILD` unsupported since macOS 10.5
  [2], as does the installed macOS 27 SDK. A process-tree snapshot can miss a
  child that forks and is reparented between observations. Discovering PIDs
  also does not establish safe identity at signal delivery or prevent another
  fork during cleanup. These are reasons not to claim complete ownership from
  a polling implementation; no such implementation was tested here.
- **Endpoint Security descendants client.** macOS 27 provides
  `es_new_descendants_client`, scoped to the caller and its descendants,
  without root or TCC approval. It still requires Apple's managed Endpoint
  Security entitlement [3, 4]. A separately signed helper could leave the Go
  binary CGO-free, but adds an artifact, a native build/signing path and an
  availability requirement to source installations. This needs an explicit
  exception to ADRs 0024/0025, not just another Go launcher.
- **Endpoint Security is not a demonstrated teardown primitive.** The API
  describes event delivery and authorization. Its sequence numbers expose
  dropped events [5]. The inspected macOS 27 SDK has `NOTIFY_FORK`, not an
  `AUTH_FORK` event. Fail-closed authorization deadlines do not recover missed
  fork notifications or prove termination on supervisor death. A helper
  proposal must solve those cases and PID reuse before a prototype is useful;
  entitlement approval alone would not establish the lifecycle gate.
- **Linux execution boundary.** A PID namespace has a documented kernel rule:
  when its init exits, remaining members are killed [6]. That is a useful
  candidate primitive for a separate container design. It still requires
  control of namespace membership, runtime ownership, mount/network policy,
  owner-death handling and completion observation. On macOS it also introduces
  a VM/runtime and Linux tooling; it does not run the host's Xcode workflow.

This review inspected documentation, SDK headers and existing code. It did not
install or run launchd jobs, an Endpoint Security helper, a container or a VM.

## Alternatives considered

| Option | Benefit | Cost and unresolved requirement |
| --- | --- | --- |
| **A. Defer native integration (recommended)** | Preserves the accepted lifecycle and source-distribution contracts; retains reproducible evidence | No native shell isolation is delivered; current bash remains permission-gated |
| B. Investigate a signed native supervisor using Endpoint Security | Preserves native macOS tools and offers descendant-scoped events | Requires Apple entitlement and distribution changes; reliable teardown, event loss and supervisor death are still unproved |
| C. Design an external Linux container/VM backend | Offers a kernel-owned process namespace and a separate execution environment | Changes the macOS-native priority, toolchain and runtime/dependency requirements; needs a separate design and host integration |
| D. Accept confinement with incomplete descendant cleanup | Could retain a small native adapter | Explicitly weakens ADR 0025; surviving work can keep changing granted files after completion, and scratch lifetime becomes unresolved; other gates still fail |
| E. Adopt another Seatbelt wrapper or App Sandbox helper | May reuse policy work or supported resource-access mechanisms | A replacement must independently prove descendant ownership; changing the wrapper or resource policy alone is not that evidence |

Options B and C authorize, at most, a separately scoped feasibility/design task
after maintainer agreement. Neither is a validated replacement. Option D is
not recommended: surviving execution would conflict with the foreground tool
contract even while it remained confined. Option E needs evidence of a cleanup
mechanism before dependency selection.

## Acceptance required for a replacement

1. Identify a process membership/identity mechanism that survives fork, exec,
   group/session changes and reparenting. Explain how a closing invocation
   prevents or captures new children and cannot target unrelated reused PIDs.
2. Define termination and completion after shell exit, timeout, cancellation,
   session close/ownership loss and agent/supervisor death, including event
   loss and partial launch failures. Completion and scratch removal must follow
   owned execution ending; a successful signal send is insufficient evidence.
3. Agree OS/architecture support, toolchain compatibility, artifacts, privileges,
   entitlement availability, installation cost and any dependency changes.
4. Retest all ADR 0025 gates with positive controls and explicit failed or
   unavailable results. Passing the lifecycle tests alone is insufficient.

## Sources

Reviewed 2026-10-22. The SDK evidence is from Xcode 27.1 (build 27A9275), installed
under `/Applications/Xcode.app/Contents/Developer`; its `EndpointSecurity`
headers are `ESClient.h`, `ESMessage.h` and `ESTypes.h`. Public XNU source is
pinned below; it is supporting documentation, not an assertion that this host
runs that exact kernel revision.

1. [Apple: creating launch daemons and agents](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html).
2. [Apple XNU: deprecated kqueue tracking flags](https://github.com/apple-oss-distributions/xnu/blob/f6217f891ac0bb64f3d375211650a4c1ff8ca1ea/bsd/sys/event.h#L334-L341).
3. [Apple: descendants client](https://developer.apple.com/documentation/endpointsecurity/es_new_descendants_client(_:_:)).
4. [Apple: required Endpoint Security entitlement](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.endpoint-security.client),
   [DTS: supported alternatives and distribution limitations](https://developer.apple.com/forums/thread/777509).
5. [Apple: event sequence numbers and dropped events](https://developer.apple.com/documentation/endpointsecurity/es_message_t/seq_num).
6. [Linux man-pages: namespace init and termination](https://man7.org/linux/man-pages/man7/pid_namespaces.7.html).
