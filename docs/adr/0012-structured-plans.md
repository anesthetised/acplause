# ADR 0012: Publish and persist item-based execution plans

- Status: accepted; snapshots and model tool implemented in #31/#32; actual T3 acceptance pending #38
- Date: 2026-10-19
- Issues: [#31](https://github.com/anesthetised/acplause/issues/31), [#32](https://github.com/anesthetised/acplause/issues/32)
- Scope change: No existing decision
- Baseline: acplause `d9c99f1`; ACP `2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`

## Context

T3 already renders ACP plan_update events. acplause only emits narrative progress, so the application's task progress UI cannot show its execution plan.

## Decisions

1. Add only pinned plan_update with plan:{type:'items',planId,entries}. Entries contain content, priority and status. Use pending/in_progress/completed and high/medium/low for outgoing values. No legacy plan event, markdown/file plan types, deletion method, or invented capability is needed.

2. Expose an agent-owned update_plan tool with {entries:[{content,priority,status}]}. Maintain one current execution plan per accepted user turn: allocate a fresh opaque planId on the first successful update, return it to the model, and reuse it for the remainder of that turn. Prior turns' plan snapshots remain in replay. Do not let the model choose another turn's ID.

3. Validate one to twenty entries, nonempty content up to 1000 characters and at most one in_progress entry. Invalid inputs leave the plan unchanged and produce a failed tool result. Each update is a complete replacement of entries, including identical updates; no parsing of prose into plans.

4. Store the latest snapshot at the plan's first-seen transcript position, using existing dirty-entry persistence. Rebuild the plan index on recovery; replay snapshots and live changes under Session.mu. Keep tool call/results separate from plan events so the model conversation remains valid.

5. Do not mark unfinished tasks completed on cancellation, failure or process restart. Plan status describes the plan, not the session lifecycle. A new turn can issue a fresh plan; prior incomplete plans retain their historical status.

6. Expose the tool in Build and Plan modes, independent of client capabilities, since plan updates are baseline optional output. Plans never run shell commands, modify files or require ACP permission.

## Consequences

One plan per turn is a small policy that still supports multiple historical plan IDs. Future cross-turn plan editing needs an explicit design change. This feature is independent of the Plan/Build execution policy in ADR 0013.

## Sources and execution

Wire shapes come only from the pinned schema and these pinned protocol pages:

- [agent-plan](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/agent-plan.mdx)
- [prompt-lifecycle](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/prompt-lifecycle.mdx)

T3 compatibility evidence: checkout `94285424c`, `packages/provider-acp/src/server/{adapter,runtimeModel,AcpSessionRuntime}.ts` and `docs/user/providers-acp.md`. This is client evidence, not a source of ACP wire shapes.

Execution plan: [plans](../../tasks/t3/plans.md). Snapshot types, validation,
per-turn identity, persistence and replay are implemented in #31. The agent-owned
`update_plan` tool is activated in #32, with in-package and schema-valid e2e
coverage for replacements, per-turn identity, invalid arguments, cancellation,
failure and restart/replay. Actual T3 acceptance remains tracked in #38.

