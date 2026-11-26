# ADR 0041: Remove unfinished plans on explicit session cancellation

- Status: accepted
- Date: 2026-11-27
- Issue: [#227](https://github.com/anesthetised/acplause/issues/227)
- Amends: decisions 1, 2, 4 and 5 of [ADR 0012](0012-structured-plans.md)
- Protocol: [ACP v2 beta.4 plan removal][removal], [cancellation][cancellation] and [replay][replay]

## Context

ACP beta.4 recommends sending `plan_removed` when a plan is cancelled.
acplause assigns each plan to one accepted turn. A client may retain its
plan ID while reconnecting and use replay to bring the plan up to date.

## Decisions

1. When `session/cancel` stops an accepted turn, remove its plan if the
   latest snapshot contains a `pending` or `in_progress` entry. Completed
   plans and plans from earlier turns retain their statuses. A deadline,
   close, disconnect, ownership loss, provider failure or process exit
   alone preserves the plan. A deadline that expires before explicit
   cancellation still preserves the plan. Withdrawing an uninserted prompt
   creates no plan event.
2. Replace the snapshot at its original transcript position. Store the
   removal using the existing `plan_update` row kind, with an envelope
   marker and an empty item snapshot for the same ID. Current agents decode
   marked rows as `plan_removed`. Older agents decode an empty snapshot.
   Historical empty snapshots without the marker retain their meaning.
3. Finalize the turn's tools, then stage the removal in a serialized,
   ownership-checked save while the session is still active. Keep the plan
   snapshot visible to replay during the save. After a successful commit
   under continuous ownership, publish one `plan_removed` with the original
   `planId` followed by `idle` with stop reason `cancelled`. On an ordinary
   save failure, retain the original snapshot, use the existing save warning
   and send the same cancelled idle. Later saves retry other dirty data and
   retain the plan. Ownership loss follows the existing retirement rules.
4. A replay cut that includes the removal emits it in history and covers
   any held copy of that live update. A cut taken earlier emits the plan
   snapshot in history and holds the removal with the live updates after
   `replay_complete`. A replacement generation replays the authoritative
   stored transcript.

## Consequences

Clients can clear a previously seen plan on replay without restoring its
entries, even after a process exits immediately after sending the removal.
The stored format needs no migration. An eligible turn waits for the removal
save before reporting idle. Its final idle state still uses the normal
post-turn save.

[removal]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/agent-plan.mdx#removing-plans
[cancellation]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/prompt-lifecycle.mdx#cancellation
[replay]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#replay-contents
