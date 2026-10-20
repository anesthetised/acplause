# ADR 0021: Bound opt-in concurrency to consecutive built-in reads

- Status: accepted; outcome/commit separation (#84) implemented, activation (#85) pending
- Date: 2026-10-20
- Issues: [#84](https://github.com/anesthetised/acplause/issues/84), [#85](https://github.com/anesthetised/acplause/issues/85)
- Extends: [ADR 0003](0003-permissions.md), [ADR 0004](0004-sqlite-storage.md), [ADR 0019](0019-scoped-project-instructions.md), [ADR 0020](0020-turn-budgets.md)

## Context

One model response can request several independent file reads. Running the
existing tool routine in goroutines would commit model history by completion
order, race toolbox replacement and let saves separate final tool cards from
matching results. Preparation must establish a small outcome boundary before
concurrency is enabled.

## Decisions

1. **Serial preparation release.** #84 extracts execution returning an outcome
   containing the original ACP entry/final patch and the model result, including
   typed images. Preparation, Plan restrictions, budget checks, permission
   revocation, MCP binding admission, forms and terminal progress keep their
   existing paths. The session worker commits each outcome exactly once before
   executing the next call. Final tool publication and model-history append
   share the session lock; flush follows outside it. No scheduler or new setting
   is enabled in this release.

2. **Narrow activation.** #85 adds `ACPLAUSE_READ_CONCURRENCY`, an integer from
   1 through 8, default 1. Only consecutive built-in `read_file`, `list_dir`,
   `search_code` and `get_instructions` calls form read groups. Eligibility uses
   exact built-in routing/names, never `ToolKind`, MCP `readOnlyHint` or a
   server-controlled annotation. Every other call is a strict barrier,
   including unknown tools, writes, `edit_batch`, bash, permissions, MCP,
   `ask_user` and `update_plan`. Drain and commit the entire preceding group
   before a barrier; finish the barrier before starting later reads. No model
   requests run concurrently or before all response calls have matching results.

3. **One group, one root snapshot.** The worker acquires one toolbox pin before
   launching a group and retains it through joining every invocation and
   committing its outcomes. All reads in that group use that immutable root
   selection, even if resume installs new roots while they run. Release retired
   roots only after the group has joined, outside the session lock. A subsequent
   group or serial call acquires the current roots. This is a root/handle
   snapshot, not a snapshot of filesystem contents: external edits may be seen.
   Workers never independently acquire/release the session's single active pin.

4. **Ordered prefix commits.** At most N reads run at once. Each invocation
   returns an outcome indexed by original model-call position; the session
   worker commits only the next contiguous ready prefix in that order. Results
   include failures and typed attachments. Completed later reads wait in memory
   for earlier siblings; do not reorder durable model history by completion.
   Keep one normal flush per committed reported call. This is a bounded batch
   executor, not a general job system.

5. **Publication and persistence.** Progress patches may interleave, always
   under the session lock so resume and live output agree. Initial activation
   defers final completion patches until the ordered prefix commit: each final
   card and its matching history result enter the same locked snapshot and
   ownership-checked save transaction. Later completion-order final publication
   would require a separate durable outcome representation and reconciliation;
   do not simply persist a completed card while buffering its result in memory.
   Publication still precedes save, as in ADR 0004: save failures retain the
   existing warning/retry behavior, and an unsaved live completion can be lost
   on process exit. This is not a stronger durability guarantee.

6. **Cancellation and failures.** Stop admitting new reads after cancellation,
   join active work, and produce exactly one result for every call, including
   not-started siblings and later barriers. Preserve existing model text:
   `Cancelled before it ran.` for a call skipped before entry, and
   `Cancelled by the user.` for cancellation observed during execution. Skipped
   calls create no new ACP card. Discard images from cancelled execution.
   A completed outcome is stable while awaiting its commit; later cancellation
   does not erase that result. A failed read cannot drop or deadlock siblings.
   Token/USD budgets are recorded on the worker before tool execution, not by
   read workers; a reached/unavailable budget starts no approval or tool work.
   The existing turn context carries timeout, close/delete and ownership loss.
   Exactly one final idle remains the turn worker's responsibility.

7. **Partial groups and crashes.** A persisted prefix retains its outcomes,
   images and final cards. Executed but uncommitted outcomes are not durable:
   their stored cards remain pending/in progress and restart marks them failed.
   Calls not yet started may have no card, but their assistant call IDs remain
   in history. Reconciliation adds one interrupted result for each unanswered
   call in original order, after the committed prefix, and never repeats work.
   Repeated repair must not duplicate results. No automatic external-call replay
   or inferred success from filesystem contents. Activation must test a crash
   during a partially completed group, including a later ready outcome blocked
   behind an earlier pending call, and roots replaced while reads are active.

8. **Evidence before activation.** #85 must prove overlap, the worker bound,
   serial barriers, ordered history, cancellation and partial-group recovery
   using deterministic barriers under the race detector. Record Go/OS/hardware
   and benchmark results for representative reads/searches at 1, 2 and 4 workers;
   use no timing-ratio CI assertions. Keep the default serial regardless of
   benchmark results. Existing MCP/permission revocation, image, terminal,
   ownership and schema-validating protocol tests remain required. No schema,
   dependency or storage migration; real T3 acceptance stays under #38/#25.

## Consequences

Independent reads can later overlap within an explicit resource bound, while
history stays deterministic. A slow first read delays later completion cards
and prefix saves. Concurrent disk-heavy reads may be slower and consume more
memory, so concurrency remains opt-in. This preparation changes no advertised
tool, setting, permission or scheduling behavior.
