# Execution plan: Publish and persist item-based execution plans

Status: planned. Baseline: acplause `d9c99f1`, 2026-10-19.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25); task checkboxes live in GitHub Issues.
Contract: [ADR 0012](../../docs/adr/0012-structured-plans.md).
Read the [roadmap](../plan.md) for ordering and shared acceptance gates.

## Execution rules

This is a planning deliverable; no feature is implemented by this document.
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

Plan data and plan execution mode are separate. This sequence can ship before Plan/Build switching.

## [#31](https://github.com/anesthetised/acplause/issues/31): Persist and replay ACP item-plan snapshots

**Depends on:** none.
**Scope:** 4 primary files; small/medium execution slice.
**Files:** `internal/acp/plan.go (new)`, `internal/agent/plan.go (new)`, `internal/agent/persist.go`, `internal/agent/session.go`.

### Ordered implementation

1. Define plan_update/PlanItems/PlanEntry from the pinned schema. Add validation for ADR 0012 limits and construct only type:items. Marshal through jsonrpc.Marshal.

2. Implement a session-owned helper that allocates a plan ID on the first update in an accepted turn, validates before mutation, upserts a complete snapshot at its first transcript position and marks it dirty under Session.mu.

3. Add encodeEntry/decodeEntry support and reconstruct indexes after loading; do not rely on ephemeral indexes being persisted. Keep old transcript rows compatible and preserve incomplete statuses on restart.

4. Add in-package live-update/resume ordering and persistence tests. This foundation does not advertise a model tool yet; it must be behavior-neutral to existing clients.

### Acceptance

- Plan replacement retains one stable ID/position per turn and distinct IDs across turns.
- Invalid updates leave stored state unchanged and snapshots survive restart.
- Cancellation/recovery never fabricates completed entries.

### Verification

```sh
go test -race ./internal/acp ./internal/agent
```

Round-trip empty historical/future fields appropriately, at most one in_progress, boundary entry counts, and competing resume/live update ordering.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## [#32](https://github.com/anesthetised/acplause/issues/32): Expose update_plan as an agent tool

**Depends on:** [#31](https://github.com/anesthetised/acplause/issues/31).
**Scope:** 3 primary files; small/medium execution slice.
**Files:** `internal/agent/plan.go`, `internal/agent/turn.go`, `internal/agent/text.go`.

### Ordered implementation

1. Define the bounded update_plan model schema and add it to the model-round catalog as an agent-owned tool. Return the allocated planId in the tool result; the model does not supply plan IDs.

2. Route execution to the plan snapshot helper and common tool lifecycle without filesystem access or permission. Invalid arguments still produce matching failed tool results.

3. Add a short system instruction explaining when to publish a plan and keep it updated. Do not require plans for every trivial task or infer a plan by parsing assistant prose.

4. Add schema-valid e2e tests for successive statuses, multiple turns, cancellation, restart/replay and malformed tool arguments. Update README and integration smoke expectations.

### Acceptance

- A scripted update_plan call produces native ACP plan_update and a valid model tool result.
- The same turn updates one plan; later turns create new IDs and replay preserves earlier snapshots.
- The tool has no workspace side effects and does not request permission.

### Verification

```sh
go test -race ./internal/agent && just e2e -run Plan
```

E2E asserts full replacement entries rather than append semantics; failed/cancelled turns retain unfinished plan statuses.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Exit checkpoint and handoff

Complete the referenced issue acceptance checklists and add the feature's actual
T3 observations under [#38](https://github.com/anesthetised/acplause/issues/38). A feature's automatic tests may pass before this
runtime check, but the roadmap remains incomplete until the runtime gate passes.
When handing off, state the last passing task, next task and any unresolved failure.

