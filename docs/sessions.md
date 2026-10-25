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

For an individual prompt request, the acceptance response precedes its
`user_message` and later turn updates. JSON-RPC batches receive one response
array after every request in the batch settles; updates may precede that array.

## List, resume, close and delete

`session/list` includes stored sessions from every process using that data
directory. `session/resume` restores their conversation history and settings.
`session/close` cancels work and releases the session in this process; its
history stays listed and can be resumed. `session/delete` cancels local work
and removes the session and its stored history. Deleting an unknown or
already deleted session succeeds. Nothing expires automatically.

Resuming a running session can replay partial messages and active tool calls.
Changes to additional directories apply to subsequent tool calls; a call
already running keeps its roots until it finishes.

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
