# ADR 0004: Store sessions in SQLite

- Status: accepted
- Date: 2026-10-19
- Issues: #8, #9
- Supersedes: ADR 0001 decision 3 for runtime code
- Source: ACP spec at `3be9cfc` (`testdata/schema/v2/VERSION`):
  `protocol/v2/session-setup.mdx#resuming-sessions`,
  `protocol/v2/session-delete.mdx`

## Context

Sessions live in memory, so `session/list` and `session/resume` lose
everything when acplause exits. Each editor window runs its own acplause
process, so several processes can share one store at the same time. The
spec leaves retention to the agent ("Agents are not required to persist
every inserted user message") and makes resuming or deleting an active
session implementation-defined.

JSON files per session would keep the standard library only, but need our
own locking between processes, atomic rewrites of whole files, and a scan
of every file for `session/list`.

## Decisions

1. **`modernc.org/sqlite`, the first runtime dependency.** It is SQLite
   translated to Go, so no cgo and no change to cross-compilation or CI.
   Measured at v1.60.1: about +6.5 MB of binary (acplause is 10.3 MB) and 10
   linked modules (`modernc.org/{sqlite,libc,memory,mathutil}`,
   `golang.org/x/sys`, `google/uuid`, `dustin/go-humanize`,
   `mattn/go-isatty`, `ncruces/go-strftime`, `remyoudompheng/bigfft`). The
   JSON Schema validator stays test-only. Other dependencies still need
   approval.
2. **One database, `~/.acplause/acplause.db`.** `ACPLAUSE_DATA_DIR` replaces
   `~/.acplause`. The directory is created `0700` and the file `0600`: it
   holds prompts, model output and tool output in plain text.
3. **Contents.** A `sessions` row per session (cwd, additional directories,
   title, timestamps, model, effort, cost, owner lease), the model history
   and the replay transcript as ordered JSON rows, and a `settings` table for
   #10. SQLite is used for sessions and settings only, not for the model
   catalog or code search.
4. **Writes at boundaries.** Prompt insertion, each completed model
   response, each tool call update, config changes and the title. Streaming
   chunks are kept in memory only; a crash loses the response being
   streamed.
5. **Recovery.** A session left mid-turn by a dead process is loaded idle.
   Tool calls without a result get one in the history ("acplause exited
   before this tool call finished") and are replayed as `failed`, so every
   tool call keeps a result.
6. **One owner per session, taken over when idle.** The lease is per
   session, so different sessions run in parallel within and across
   processes; only one process works on a given session. A process owns the
   sessions it created or resumed until `session/close` or exit, through a
   lease (owner id and expiry) renewed every 10 s and valid for 30 s, next
   to the session's foreground state.
   - `session/resume` of a session another process owns takes it over when
     that session is `idle`, and fails with `-32603` ("session is busy in
     another acplause process") while it is `running` or
     `requires_action`. An expired lease is taken over in any state.
   - Inserting a prompt checks ownership and records `running` in one
     transaction, so a takeover and a new turn cannot both win.
   - The previous owner notices at its next lease renewal or write, drops
     the session and rejects later prompts to it with `-32603` ("session
     was resumed in another acplause process"). It writes nothing after
     losing the lease.
   - `session/list` shows every stored session.
7. **Retention until `session/delete`.** Nothing expires on its own.
   `session.delete` is advertised; deleting removes the rows. Deleting an
   unknown or already deleted session succeeds silently (SHOULD). Deleting
   a session open in this process cancels and closes it first; one open in
   another process is deleted on the same terms as a takeover.
8. **Schema versions.** `PRAGMA user_version` with forward-only migrations in
   code. A database newer than the binary is refused at startup instead of
   being downgraded.
9. **Connection.** WAL journal, `busy_timeout` of 5 s, foreign keys on, one
   `*sql.DB` per process. The agent depends on a small `agent.Store`
   interface implemented in `internal/store`, so agent tests keep fakes.
10. **Failures.** acplause refuses to start if the database cannot be opened
    or migrated. A failed write during a session is logged and the session
    continues from memory; that change may be missing after a restart.

## Consequences

- Sessions survive restarts and are shared by all acplause processes of the
  user.
- The binary grows by about 60%, and builds compile the SQLite translation
  once per module cache.
- A session left idle in one editor window can be continued in another; the
  first window's next prompt to it fails. A session that is working in one
  window cannot be taken until it is idle, or its process has been gone for
  30 s.
- Conversations, including tool output, stay on disk until deleted.
