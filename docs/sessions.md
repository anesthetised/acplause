# Sessions and storage

[Documentation](README.md) · [Project overview](../README.md)

## Storage and privacy

Sessions are stored in `~/.acplause/acplause.db`; `ACPLAUSE_DATA_DIR` selects
another directory. acplause creates the directory with permissions `0700`
and the database with `0600` on Unix. On Windows the default directory is
`%USERPROFILE%\.acplause`; file access follows inherited Windows ACLs, not Unix
mode bits. Choose a private user directory for `ACPLAUSE_DATA_DIR`.
The database holds prompts, model output and
tool output in plain text, along with workspace paths, titles, timestamps,
model and effort selections, and accumulated cost. Prompt and tool images are
stored inline in model history and the original ACP transcript until session deletion;
these copies increase database size, and there is no session-wide storage
quota. See [ADR 0004](adr/0004-sqlite-storage.md).

## Session metadata and prompt acceptance

A session is titled after the first line of its first prompt. The title is
sent once, as a `session_info_update`, when that prompt is accepted, and is
returned by `session/list`. It is not replayed on resume: replay is
conversation history, and the title is metadata that `session/list` reports.

Send one JSON-RPC object per line and match replies by ID. A client can send
the next prompt or cancellation while a reply is pending. An accepted prompt's
`messageId` reply precedes its `user_message` echo and later turn updates.
A top-level array receives one `-32600` error with `id: null`. None of its
entries run. [ADR 0032](adr/0032-reject-jsonrpc-batches.md)

A client can send an optional `idempotencyKey` with `session/prompt`. Use the
same key for each attempt at one submission. Pending retries share the result,
and a retry after insertion returns the original `messageId` without another
turn. Cancelling any pending attempt withdraws the whole submission before
insertion. The key is private to the session and survives restart and context
compaction with its user message. If a save warning preceded a restart, both
the unsaved message and its key may have been lost.
See [ADR 0033](adr/0033-prompt-idempotency-keys.md).

## List, resume, close and delete

`session/list` includes stored sessions from every process using that data
directory. `session/resume` restores their conversation history and settings.
When opening a session from disk, its usable saved model takes precedence over
the [launcher default](configuration.md#default-model). A successful fallback
saves the replacement on the session. When neither choice is usable, resume
fails and keeps the saved session and history available for retry.
`session/close` cancels work and releases the session in this process. Its
pending permission cancellations and final foreground updates precede the reply.
A cancelled close replies with `-32800` after cleanup and lease release. The
history stays listed and can be resumed. Closing a missing or inactive session
succeeds. `session/delete` cancels local work and removes the session and its
stored history. Deleting an unknown or already deleted session succeeds.
Nothing expires automatically.

Resuming a running session can replay partial messages and active tool calls.
Changes to additional directories apply to subsequent tool calls; a call
already running keeps its roots until it finishes.

A cursor resume can return acplause error `1001` when an earlier operation
still owes updates for that session. Retry after the earlier operation
finishes. If live updates exceed the replay buffer, acplause returns a
resume error, delivers the buffered live updates in order, and keeps the
connection open. A fresh cursor can request history again.
[ADR 0035](adr/0035-resume-write-and-replay-admission.md)

## Multiple windows and ownership

Two editor windows can work on different sessions at the same time. Resuming
or deleting a session owned by another window is allowed while it is idle,
and refused while it is working or waiting for permission or form answers.
After a takeover or deletion, the old window loses access at its next write or lease renewal
(every 10 seconds). A crashed process's lease expires within 30 seconds;
a resumed session then starts idle, with unfinished tool calls marked failed
and matching results added to the model history.

## Save boundaries and failures

Changes are saved at prompt, model response, tool call and configuration
boundaries. Streaming chunks stay in memory until a save boundary, so a crash
can lose the response being streamed. Failure to open or migrate the database
prevents startup. A later save failure is logged to stderr and sends a live ACP
warning notice; work continues from memory, and unsaved changes may be missing
after a restart. Repeated failures send no additional notice until a save succeeds.
Notices are not stored or replayed, and clients may ignore them.

Explicit cancellation can remove the active turn's unfinished plan. The
agent saves the removal before reporting it, so it survives a restart even
if the process exits before saving the turn's idle state. If the removal
save fails, the previous plan stays visible and the turn still reports
cancelled. [ADR 0041](adr/0041-remove-explicitly-cancelled-plans.md)
