# Execution plan: Expose a restrictive Plan mode through session configuration

Status: implemented for #33/#34 with automated coverage; real-T3 gate (#38) pending.
Baseline: acplause `d9c99f1`, 2026-10-19.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25); task checkboxes live in GitHub Issues.
Contract: [ADR 0013](../../docs/adr/0013-plan-build-mode.md).
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

Do not expose mode selection until the invocation guard is implemented. ASK and PLAN prerequisites define the full allowlist for Plan mode.

## [#33](https://github.com/anesthetised/acplause/issues/33): Persist session execution mode without changing current behavior

**Depends on:** none.
**Scope:** 3 primary files; small/medium execution slice.
**Files:** `internal/store/store.go`, `internal/agent/persist.go`, `internal/agent/session.go`.

### Ordered implementation

1. Append a SQLite mode column migration with default build and update Session row reads/writes. Preserve all earlier migrations and transactional ownership semantics.

2. Carry mode through agent record/recovery, default old rows and new sessions to build, and keep it out of globally remembered model/reasoning preferences.

3. Add migration tests from the previous user_version, independent sessions, resume/takeover and failure cases. Do not expose a selectable Plan option until enforcement is complete in MODE.

4. Verify old DB fixtures, leases and all existing tests. This slice only adds inert durable state.

### Acceptance

- Old and new sessions have build mode with no behavior change.
- Per-session mode round-trips without changing global model/effort selection.
- Migration and session ownership checks remain atomic and backward-load compatible.

### Verification

```sh
go test -race ./internal/store ./internal/agent
```

Use old database creation fixtures rather than recreating the latest schema and calling that a migration test.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## [#34](https://github.com/anesthetised/acplause/issues/34): Wire T3 Plan/Build configuration to enforced tool policy

**Depends on:** [#33](https://github.com/anesthetised/acplause/issues/33), [#30](https://github.com/anesthetised/acplause/issues/30), [#32](https://github.com/anesthetised/acplause/issues/32).
**Scope:** 5 primary files; small/medium execution slice.
**Files:** `internal/agent/catalog.go`, `internal/agent/session.go`, `internal/agent/agent.go`, `internal/agent/turn.go`, `internal/agent/text.go`.

### Ordered implementation

1. Expose mode as a schema-valid select with build/plan values and category mode in all setup/config responses. Skip global rememberSelection for mode-only changes. Never use v1 set_mode.

2. Make idle-only changes atomic with prompt admission and lifecycle setup reservations. Reject changing mode while active/queued/setup is pending; same-value is idempotent. Rebuild system instructions with AGENTS.md/roots intact.

3. Apply the same immutable mode policy to round tool definitions and invocation admission. Plan permits only bounded built-in reads plus ask_user/update_plan; all bash/write/edit/MCP attempts fail before approval/side effects, regardless of MCP readOnlyHint.

4. Add end-to-end T3-shaped set_config_option sequences for Plan then Build, persistence/resume, denied stale calls and a pending permission. Update README policy and document the busy-transition retry requirement.

### Acceptance

- Plan/Build is selectable through standard config and mode survives restart.
- Plan prevents forbidden execution even for a hallucinated/stale call and still records every result.
- Build preserves existing client-owned approvals; live mode changes cannot race active work.

### Verification

```sh
go test -race ./internal/agent ./internal/store && just e2e -run 'Mode|Plan'
```

Explicitly test a falsely readOnlyHint MCP tool, bash disguised as read, queued prompt and concurrent setup, plus unchanged model/effort preferences.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Exit checkpoint and handoff

Complete the referenced issue acceptance checklists and add the feature's actual
T3 observations under [#38](https://github.com/anesthetised/acplause/issues/38). A feature's automatic tests may pass before this
runtime check, but the roadmap remains incomplete until the runtime gate passes.
When handing off, state the last passing task, next task and any unresolved failure.

