# Execution plan: Ask structured questions through ACP form elicitation

Status: #29 and #30 implemented; actual T3 acceptance pending #38. Baseline: acplause `d9c99f1`, 2026-10-19.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25); task checkboxes live in GitHub Issues.
Contract: [ADR 0011](../../docs/adr/0011-structured-user-questions.md).
Read the [roadmap](../plan.md) for ordering and shared acceptance gates.

## Execution rules

This document records the execution contract; implementation status is stated above.
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

Form elicitation is a direct ACP client capability. Do not enable MCP server elicitation or modify the permission protocol as a shortcut.

## [#29](https://github.com/anesthetised/acplause/issues/29): Add opt-in cancellation for outbound ACP requests

**Depends on:** none.
**Scope:** 2 primary files; small/medium execution slice.
**Files:** `internal/jsonrpc/conn.go`, `internal/agent/agent.go`.

### Ordered implementation

1. Add a named CallCancellable method sharing the existing pending-call implementation with Call; ordinary Call must keep its current permission semantics. Keep the small agent-side client contract explicit and update its test fakes.

2. When a successfully enqueued request is locally cancelled, queue $/cancel_request with its actual requestId through the single writer. Serialize enqueue order and guard races against response completion/connection shutdown.

3. Make cancellation cleanup nonblocking even when the write queue is full. If cancellation cannot be queued promptly, abandon the local wait; do not close the whole connection or block session/cancel. Do not emit a cancel for a request never enqueued.

4. Add transport-level tests for response winning, cancellation winning, late response, output backpressure, connection close and ordinary permission Call staying unchanged.

### Acceptance

- Cancellable outbound requests terminate promptly and send best-effort wire cancellation with the correct ID.
- Cancellation cannot overtake its request or reanimate a completed waiter.
- Existing permission Call and incoming $/cancel_request behavior remain covered and unchanged.

### Verification

```sh
go test -race ./internal/jsonrpc ./internal/agent
```

Run race tests with a deliberately blocked writer and multiple concurrent outbound requests; use synchronization barriers, not sleeps.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## [#30](https://github.com/anesthetised/acplause/issues/30): Expose ask_user through ACP form elicitation

**Depends on:** [#29](https://github.com/anesthetised/acplause/issues/29).
**Scope:** 6 primary files; small/medium execution slice.
**Files:** `internal/acp/elicitation.go (new)`, `internal/acp/initialize.go`, `internal/agent/agent.go`, `internal/agent/elicitation.go (new)`, `internal/agent/turn.go`, `internal/agent/session.go`.
The shared `deps` in session.go carries connection-local form capability into
both new and restored sessions; it adds no persisted state or independent feature.
The e2e validator also recognizes pinned `x-side: protocol` notifications so
outbound `$/cancel_request` receives definition-level validation.

### Ordered implementation

1. Define only emitted/read form types from the pinned schema and save the negotiated client form capability. Add tests for omitted/null/form-empty/unknown capabilities. No arbitrary JSON Schema framework or MCP elicitation support.

2. Add agent-owned ask_user tool and validate ADR 0011's bounded string/enum question arguments. Map them to flattened elicitation/create scope and required string properties. Keep filesystem Toolbox independent.

3. Integrate the tool through common tool lifecycle, CallCancellable, requires_action/running transitions and matching history results. Validate answer keys/types/enums; decline/cancel/error/unknown response is never success or permission.

4. Extend the e2e harness to explicitly answer or cancel elicitation requests while preserving automatic permission behavior. Cover cancelled/late responses, same-connection resume, process restart and a client without form capability. Update README and the smoke procedure.

### Acceptance

- A negotiated form client receives one schema-valid form and its validated answers reach the model as a tool result.
- No-capability clients get no form tool; invalid answers and decline/cancel have explicit failed results.
- Cancel/close/delete/lease loss cannot leave foreground work or a late answer running.

### Verification

```sh
go test -race ./internal/acp ./internal/agent ./internal/jsonrpc && just e2e -run Elicitation
```

Test three questions, invalid fourth question, duplicate IDs/options, required field missing, enum mismatch and no automatic reissue after resume.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Exit checkpoint and handoff

Complete the referenced issue acceptance checklists and add the feature's actual
T3 observations under [#38](https://github.com/anesthetised/acplause/issues/38). A feature's automatic tests may pass before this
runtime check, but the roadmap remains incomplete until the runtime gate passes.
When handing off, state the last passing task, next task and any unresolved failure.

