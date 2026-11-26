# ADR 0033: Retain prompt retry keys on user-message transcript rows

- Status: accepted
- Date: 2026-11-26
- Issue: [#218](https://github.com/anesthetised/acplause/issues/218)
- Protocol: [ACP v2 beta.4, Retrying a prompt](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/prompt-lifecycle.mdx#retrying-a-prompt)

## Context

A client that loses a prompt response can retry with the same session and
`idempotencyKey`. It needs the original client-visible message ID for as long
as the agent retains the inserted message. The replay transcript outlives
model history compaction under [ADR 0006](0006-context-compaction.md).

## Decision

1. Add a nullable key column to the existing transcript row. Store the exact
   UTF-8 bytes as a SQLite BLOB with a partial unique index on session and
   non-NULL key. This preserves case, combining characters and embedded NUL.
   Existing rows migrate with NULL. Omission and JSON null request an unkeyed
   insertion. A supplied string must contain 1–255 Unicode code points.
2. Keep the existing random message ID and its transcript position. The
   ownership-checked transaction that inserts the history, user-message row
   and running state also records the key. Dirty saves carry the original
   key with the row. A conflict rolls back the whole save and a fresh resume
   restores authoritative state. A separate keys table would require a
   second row and a separate retention relationship for each keyed message.
3. Join requests for a pending key at receipt, including while the first
   request waits for setup. The original request supplies content and queue
   position. Every attempt has its own reply. Cancellation of any pending
   request withdraws the group before insertion. Pre-insertion errors free
   the key. If the commit already succeeded, every waiter receives the
   committed ID, even when its request was cancelled during result delivery.
   A recorded key returns that ID without comparing content or starting work.
4. Acquisition loads keys with the transcript in the lease transaction.
   Restoration indexes each key by the ID in its user-message payload.
   Compaction retains those rows. Session deletion removes them through the
   existing foreign key. Keep keys private to admission and storage.
5. Preserve [ADR 0004](0004-sqlite-storage.md) and
   [ADR 0016](0016-save-failure-notices.md) for ordinary storage failures.
   Insert the message and key together in memory, issue the existing warning,
   and retry both at the next save. A process exit before any successful save
   can lose both. Cancellation, ownership loss and identity conflicts reject
   the insertion before it becomes visible.

## Consequences

Retries can reconcile a lost response after reconnect, restart or compaction.
The partial index and in-memory lookup grow with retained keyed messages.
Legacy messages remain replayable with their original IDs. Unkeyed prompts
continue to create separate messages, including when their contents match.
