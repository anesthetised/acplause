# Execution plan: Stream agent-owned bash terminals with bounded replay

Status: implemented for #35/#36 with automated coverage; real-T3 gate (#38) pending.
Baseline: acplause `d9c99f1`, 2026-10-19.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25); task checkboxes live in GitHub Issues.
Contract: [ADR 0014](../../docs/adr/0014-stream-agent-terminals.md).
Read the [roadmap](../plan.md) for ordering and shared acceptance gates.

## Execution rules

This document records the implementation contract and verification workflow.
Use GitHub issues as the authoritative task checklist. Each issue below is one
bounded execution task; do not implement the whole roadmap in one run.

1. Read AGENTS.md, the linked ADR, and the named files. Confirm dependency
   issues are implemented in the checkout, not merely closed in another branch.
2. Run the focused baseline command. Add a regression or behavior test, implement
   the smallest slice, then run the focused command again. Proposed test names
   should be created, not treated as evidence that coverage already exists.
3. Preserve stdout framing, insertion boundary, one idle per turn, complete tool
   results, locking/replay order, ownership and MCP binding invariants.
4. No dependency additions, schema refresh, unrelated refactoring or T3-specific
   routing. If the pinned schema or actual code contradicts the contract, record
   the concrete conflict and revise the design explicitly before changing behavior.
5. Keep changes to the named production files, associated tests and the
   explicitly required README/AGENTS/ADR status updates; new files are marked. If another production area is required, explain the dependency
   and split it into a follow-up slice rather than silently broadening this issue.
6. Run `just check` and `just test -coverprofile=cover.out`, then
   `go tool cover -func=cover.out | tail -1`; statement coverage must remain >=80%.
   E2E must validate emitted messages against the existing pinned schema.
7. Report the changed behavior, tests, limitations and issue/commit. Never check
   off a real-T3 gate on fake-client evidence. Commit locally when checks pass;
   push only on an explicit user request. No automatic PR merge or issue closure.

All task sizes below count primary production files; add focused test files
next to the behavior and protocol-visible cases under e2e. If a feature exceeds
one focused session, stop at its tested dependency checkpoint and hand off the
remaining exact step. Do not weaken the contract to make it fit a smaller model.

## Entry checkpoint

Keep bash permission, timeout and process-group tests green. Output observation is internal until TERMINAL is implemented.

## [#35](https://github.com/anesthetised/acplause/issues/35): Expose bounded streaming events from bash execution

**Depends on:** none.
**Scope:** 4 primary files; small/medium execution slice.
**Files:** `internal/tools/bash.go`, `internal/tools/tools.go`, `internal/tools/proc_unix.go`, `internal/tools/proc_other.go`.

### Ordered implementation

1. Add an invocation-local optional shell observer for start/output/exit. Keep it independent of ACP and retain existing final textual output and default options.

2. Emit start only after a successful process start, stream raw combined stdout/stderr bytes and report real exit code/signal or unknown completion. Preserve process-group cancellation and WaitDelay behavior.

3. Bound copied output to OutputLimit, keep omitted-byte count, and ensure callback/observer publication cannot block subprocess draining on ACP I/O. Do not introduce shared observers across sessions.

4. Add tests proving bytes arrive before process exit using explicit barriers, split UTF-8/binary bytes, large output, start failure and cancellation. Unix signal tests stay behind the unix build tag.

### Acceptance

- Output becomes observable before completion while old no-observer behavior remains unchanged.
- Observer memory is bounded and does not prevent cancellation/process cleanup.
- Start/exit signals are correct for success, failure, timeout and platform differences.

### Verification

```sh
go test -race ./internal/tools
```

Use slow and failing consumers, concurrent commands in separate sessions and the existing process-tree cleanup tests.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## [#36](https://github.com/anesthetised/acplause/issues/36): Publish and replay ACP terminal output for bash tools

**Depends on:** [#35](https://github.com/anesthetised/acplause/issues/35).
**Scope:** 5 primary files; small/medium execution slice.
**Files:** `internal/acp/terminal.go (new)`, `internal/acp/update.go`, `internal/agent/terminal.go (new)`, `internal/agent/turn.go`, `internal/agent/persist.go`.

### Ordered implementation

1. Define terminal_update/output_chunk and terminal tool-content reference from the pinned schema. Use pointer exitCode to retain zero and base64 byte frames, never raw bytes in JSON.

2. Connect permitted bash execution to a per-invocation terminal pump with coalesced wakeups, 100ms publish interval and 16 KiB chunk bound. The writer callback does no ACP I/O and takes no session lock; no unbounded backlog.

3. Maintain a bounded authoritative terminal transcript snapshot under Session.mu, persist at existing boundaries and replay once. Stop/join pump before exit snapshot and final tool status. Recover unfinished terminals as ended with unknown status plus the existing failed tool result.

4. Add e2e live-before-exit assertions, final output replacement, denied/start-failed cases, split bytes, no post-final chunks, cancellation, resume during output and restart. Update README to describe agent-owned, noninteractive terminals.

### Acceptance

- T3 receives command/cwd, linked terminal output before exit, and a correct final exitStatus.
- Replay is bounded and ordered with live chunks without duplication.
- Permission denial creates no process/terminal and cancel/close cannot leak a pump.

### Verification

```sh
go test -race ./internal/acp ./internal/agent ./internal/tools && just e2e -run Terminal
```

Coordinate resume and command output with channels/barriers; assert terminal finalization precedes tool completion and the one idle state.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Integration note

The activation slice also carries a small `preparedTool` adapter change in
`internal/agent/mcp.go`, where built-in calls enter the shared execution path.
It retains the invocation-local observer hook; external MCP calls do not use
it. This glue is required to connect SHELL to TERMINAL without moving the
existing preparation code.

## Exit checkpoint and handoff

Complete the referenced issue acceptance checklists and add the feature's actual
T3 observations under [#38](https://github.com/anesthetised/acplause/issues/38). A feature's automatic tests may pass before this
runtime check, but the roadmap remains incomplete until the runtime gate passes.
When handing off, state the last passing task, next task and any unresolved failure.

