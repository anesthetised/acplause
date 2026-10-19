# Execution plan: Advertise and execute a deterministic /compact command

Status: implemented with automated protocol tests; actual T3 acceptance remains pending under #38.
Planning baseline: acplause `d9c99f1`, 2026-10-19.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25); task checkboxes live in GitHub Issues.
Contract: [ADR 0015](../../docs/adr/0015-compact-slash-command.md).
Read the [roadmap](../plan.md) for ordering and shared acceptance gates.

## Execution rules

The implementation follows this execution contract; runtime client acceptance
is recorded separately under #38.
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

Image-aware history must be in place before force compaction is enabled. Reuse the existing compaction algorithm.

## [#37](https://github.com/anesthetised/acplause/issues/37): Advertise /compact and execute it through the prompt queue

**Depends on:** [#27](https://github.com/anesthetised/acplause/issues/27).
**Scope:** 5 primary files; small/medium execution slice.
**Files:** `internal/acp/session.go`, `internal/agent/agent.go`, `internal/agent/commands.go (new)`, `internal/agent/session.go`, `internal/agent/compact.go`.

### Ordered implementation

1. Add availableCommands in setup response types and return one static compact command from both new and resume. Do not emit a fake dynamic command update.

2. Recognize the exact single-text-block command and dispatch after normal insertion on the session worker. Ordinary slash text, resource mentions and mixed attachments must keep the normal prompt path.

3. Factor an explicit force option into the existing compaction algorithm and share summary validation/budget/cancellation/persistence. Preserve image-aware grouping. Do not call the normal tool loop after a command; return a concise confirmation or no-op message.

4. Add e2e command discovery, below-threshold forced compaction, insufficient history, unknown context window, cancellation before/after insertion, error, persistence and unchanged full replay. Update README examples.

### Acceptance

- T3 can discover /compact immediately on new and resume.
- An accepted command compacts eligible history deterministically without normal tool execution.
- Queue, insertion boundary, one idle state, usage accounting and replay remain correct.

### Verification

```sh
go test -race ./internal/acp ./internal/agent && just e2e -run 'Command|Compact'
```

Test /compactness, /compact with args, an embedded resource saying /compact, mixed image prompt and summary failure under a finite round budget.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Exit checkpoint and handoff

Complete the referenced issue acceptance checklists and add the feature's actual
T3 observations under [#38](https://github.com/anesthetised/acplause/issues/38). A feature's automatic tests may pass before this
runtime check, but the roadmap remains incomplete until the runtime gate passes.
When handing off, state the last passing task, next task and any unresolved failure.

