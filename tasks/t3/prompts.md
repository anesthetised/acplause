# Execution plan: Accept user image prompts, including T3 clipboard attachments

Status: planned. Baseline: acplause `d9c99f1`, 2026-10-19.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25); task checkboxes live in GitHub Issues.
Contract: [ADR 0010](../../docs/adr/0010-image-prompts.md).
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

MEDIA and HISTORY must be implemented first. MCP image activation is not a dependency. Clipboard capture belongs to T3; test the image block actually sent over ACP.

## [#28](https://github.com/anesthetised/acplause/issues/28): Accept user image prompts and T3 clipboard screenshots

**Depends on:** [#27](https://github.com/anesthetised/acplause/issues/27).
**Scope:** 5 primary files; small/medium execution slice.
**Files:** `internal/acp/content.go`, `internal/acp/initialize.go`, `internal/agent/agent.go`, `internal/agent/session.go`, `internal/agent/text.go`.

### Ordered implementation

1. Add parsed fields for ACP image blocks while preserving raw JSON echo. Reuse media validation; complete base64/image decoding outside Session.mu and before durable insertion. Add focused tests for unknown fields, image-only prompts, MIME/size failures and text/resources mixed with images.

2. At insertion, recheck the selected model and validation result after all earlier setup/config operations. Reject unsupported images/model before messageId, user_message, history or provider I/O. Cover queued model changes and cancel racing validation/insertion.

3. Build the canonical user message with text plus typed attachments; use Image prompt as the image-only title. Add process-level session.prompt.image advertisement only once the entire path is covered. Model-specific errors remain explicit; do not advertise vision per session with an invented update.

4. Add end-to-end restart/resume and subsequent model-switch tests, README clipboard instructions and scope updates. Record the actual paste smoke under the integration issue: a thumbnail or path string is insufficient.

### Acceptance

- T3 can submit pasted/uploaded PNG/JPEG attachments and a known-vision model receives their pixels.
- Invalid input or a non-vision selection fails before insertion, with no model substitution.
- Image-only/multiple-image prompts echo verbatim and survive restart/resume.

### Verification

```sh
go test -race ./internal/acp ./internal/agent && just e2e -run 'ImagePrompt|Clipboard'
```

Use generated tiny valid image fixtures and separate oversized/header-only fixtures; test exact image count/byte limits and absence of image data in titles.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Exit checkpoint and handoff

Complete the referenced issue acceptance checklists and add the feature's actual
T3 observations under [#38](https://github.com/anesthetised/acplause/issues/38). A feature's automatic tests may pass before this
runtime check, but the roadmap remains incomplete until the runtime gate passes.
When handing off, state the last passing task, next task and any unresolved failure.

