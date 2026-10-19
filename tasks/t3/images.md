# Execution plan: Carry bounded image tool results through model history

Status: planned. Baseline: acplause `d9c99f1`, 2026-10-19.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25); task checkboxes live in GitHub Issues.
Contract: [ADR 0009](../../docs/adr/0009-image-tool-results.md).
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

Text-only MCP works today. Neither these groundwork issues nor tool images authorize advertising user prompt image support.

## [#26](https://github.com/anesthetised/acplause/issues/26): Add bounded image values and Chat Completions image projection

**Depends on:** none.
**Scope:** 3 primary files; small/medium execution slice.
**Files:** `internal/media/image.go (new)`, `internal/llm/types.go`, `internal/llm/client.go`.

### Ordered implementation

1. Implement PNG/JPEG validation and immutable typed image values with ADR 0009 byte, dimension and count bounds. Test malformed base64, MIME mismatch, truncated image, oversized dimensions and exact limits without allocating the claimed pixel area.

2. Add optional typed attachments to canonical llm.Message while retaining Content string and old JSON compatibility. Read architecture.input_modalities in Model and add an explicit known-vision predicate; keep tool support filtering unchanged.

3. Create separate upstream chat wire message projection. Text-only requests retain current shape. For tool images, finish the full assistant/tool group before inserting a labeled synthetic user image message. Handle multiple sibling calls, empty text, mixed text/images and tool_call_id attribution.

4. Test stored old/new JSON round trips and exact HTTP request bodies. Internal metadata must not appear upstream; data URLs occur only inside image_url parts. Do not enable ACP image capabilities or MCP image output in this issue.

### Acceptance

- Valid PNG/JPEG data projects as image_url parts for explicit vision models; invalid inputs fail with bounded allocations.
- All sibling tool results precede synthetic image context, with original call IDs intact.
- Text-only API/storage fixtures still work; canonical history contains no synthetic user turns.

### Verification

```sh
go test -race ./internal/media ./internal/llm
```

Test unknown input_modalities, multiple image-producing tools, no image bytes in ordinary text, and model projection that does not mutate stored messages.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## [#27](https://github.com/anesthetised/acplause/issues/27): Make persistence and context compaction image-aware

**Depends on:** [#26](https://github.com/anesthetised/acplause/issues/26).
**Scope:** 4 primary files; small/medium execution slice.
**Files:** `internal/agent/compact.go`, `internal/agent/persist.go`, `internal/agent/catalog.go`, `internal/agent/turn.go`.

### Ordered implementation

1. Add a single model-request image policy at prepareRequest: known-vision projection, four-image/8 MiB request caps and explicit omission markers for a non-vision model. Snapshot model and image policy together; do not use the permissive generic Supports helper to infer vision.

2. Replace byte-counting of attachment base64 with ADR 0009's explicit image reserve. Remove oldest unprotected images only in model context; protect latest real user prompt and latest complete tool exchange. Keep canonical tool/result grouping and image labels through budget calculations.

3. Sanitize summary input before JSON serialization, replacing image bytes with descriptors. Ensure an image-only protected exchange fails explicitly if it cannot fit; no automatic model switch or silent deletion.

4. Exercise existing JSON-row persistence with typed attachments and replay image content, including old text-only DB fixtures, failed save, restart recovery, lease loss, context trimming and selected-model changes. Keep image activation disabled until this slice passes.

### Acceptance

- No summary request or text token estimate contains base64; count and byte caps apply across the full request.
- Restart/resume preserves validated attachments and old sessions load unchanged.
- Trimming changes model context only and never breaks a tool/result pair or the protected latest prompt.

### Verification

```sh
go test -race ./internal/agent ./internal/store ./internal/llm
```

Use in-package fake Model tests to inspect normal and summary requests; add matching e2e assertions when images become externally reachable in #23/PROMPT.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## [#23](https://github.com/anesthetised/acplause/issues/23): Support image results from MCP tools

**Depends on:** [#27](https://github.com/anesthetised/acplause/issues/27).
**Scope:** 4 primary files; small/medium execution slice.
**Files:** `internal/acp/content.go`, `internal/agent/mcp.go`, `internal/agent/turn.go`, `e2e/testdata/mcp/main.go`.

### Ordered implementation

1. Add schema-derived ACP image content encoding, preserving raw input block behavior. Validate MCP image blocks only at projection; do not change raw transport fidelity or the 8 MiB frame cap.

2. Project usable images into typed tool attachments and ACP tool_call_update content. Keep text/structuredContent and opaque IDs intact. rawOutput includes image descriptors rather than another base64 copy; retain existing audio/blob omission behavior.

3. Use the model selected for the round and reapply request-time vision policy if selection changes later. Unsupported model/image handling follows ADR 0009; no repeat of a tools/call after a result projection error.

4. Add fake-server text+image and image-only e2e coverage, then restart/resume and compaction tests using the shared foundation. Update README/AGENTS image scope and ADR 0007 cross-reference only to the extent implemented. User prompt image capability remains absent.

### Acceptance

- A fake MCP screenshot reaches the vision-model request and schema-valid ACP content exactly once.
- Invalid/oversized images and non-vision models have explicit outcomes without losing text or replaying calls.
- History/replay retains supported images after restart; no bytes leak into logs, summary text or duplicated rawOutput.

### Verification

```sh
go test -race ./internal/acp ./internal/agent && just e2e -run MCP
```

Include cancellation in a multi-tool response, isError with images, result containing only unsupported image data and retained tool_call_id after projection.

Then run the shared check/coverage gate. For groundwork issues, no public feature
is advertised yet; their dependent activation issue supplies protocol e2e coverage.

## Exit checkpoint and handoff

Complete the referenced issue acceptance checklists and add the feature's actual
T3 observations under [#38](https://github.com/anesthetised/acplause/issues/38). A feature's automatic tests may pass before this
runtime check, but the roadmap remains incomplete until the runtime gate passes.
When handing off, state the last passing task, next task and any unresolved failure.

