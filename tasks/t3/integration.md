# Execution plan: real T3 acceptance

Status: planned. Tracker: [#25](https://github.com/anesthetised/acplause/issues/25). Baseline client evidence: T3 checkout
`94285424c`; record the actual installed build during testing.

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

## [#38](https://github.com/anesthetised/acplause/issues/38): Verify first-class ACP workflows in actual T3 Code

**Depends on:** [#23](https://github.com/anesthetised/acplause/issues/23), [#28](https://github.com/anesthetised/acplause/issues/28), [#30](https://github.com/anesthetised/acplause/issues/30), [#32](https://github.com/anesthetised/acplause/issues/32), [#34](https://github.com/anesthetised/acplause/issues/34), [#36](https://github.com/anesthetised/acplause/issues/36), [#37](https://github.com/anesthetised/acplause/issues/37).
**Scope:** 3 primary files; small/medium execution slice.
**Files:** `docs/testing/t3-first-class-smoke.md (new)`, `README.md`, `AGENTS.md`.

### Ordered implementation

1. Record agent commit, T3 version/build, transport, selected model IDs and completed issue versions. Run the built acplause binary as a T3 ACP provider; native parent tools or CLI fallback cannot substitute for child-provider behavior.

2. Test clipboard PNG with a random visual marker, file-upload JPEG, image-only/multiple images, text-only model rejection; then browser-tool screenshot recognition. Record input source separately from tool images and ensure answer text was not given in the prompt.

3. Exercise form answers/cancel, plan replacement, Plan/Build restrictions, a streaming long command cancelled midway, /compact, and process restart/resume of images/plans/terminal snapshots. Include async MCP delegation after these features to catch regression.

4. Write observations and failures honestly, with secret-free metadata only. If T3 cannot render a schema-valid image, separate agent/model delivery from client display and file a client issue; do not alter wire shapes. Do not mark the roadmap complete while a required runtime gate is unavailable.

### Acceptance

- Every required actual-client workflow has reproducible observed evidence, including clipboard-to-vision.
- Automated schema/race checks pass and total measured Go statement coverage remains at least 80%.
- README/AGENTS describe shipped support and remaining limitations without claiming native fork/rewind/steering parity.

### Verification

```sh
just check && just test -coverprofile=cover.out && go tool cover -func=cover.out | tail -1
```

Checkpoint rollback, native subagent lineage, active steering and auth are explicitly outside this ACP v2 roadmap; existing MCP smoke must still pass.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Minimal runtime record

Record date, environment, agent commit, T3 version, model, transport and a
pass/fail/blocked observation for every feature. Include sanitized evidence of
ACP capability and event shapes without prompts containing private data,
credentials, authorization headers, endpoint secrets or image base64.

Use a temporary workspace and locally generated PNG/JPEG fixtures. The clipboard
case must use actual paste in T3; file upload is a second case, not a replacement.
Identify a random visible marker to prove pixels reached the selected model.
Record whether the UI renders the attachment, whether ACP carries it, and whether
the model recognizes it as three distinct observations.

A runtime failure creates a concrete blocker issue linked from [#25](https://github.com/anesthetised/acplause/issues/25); keep
this acceptance issue open. Do not claim compatibility on the basis of native
Codex tools or the parent's MCP calls.

