# Concurrency model

[Documentation](README.md) · [Project overview](../README.md)

This explains how the code upholds the
[invariants in AGENTS.md](../AGENTS.md#invariants): which locks exist, the
order they are taken in, which goroutines run, who answers each request, and
how resume, lease loss and shutdown meet a running turn. Code is cited by file
and function; comments next to the code remain the detailed reference.

## Locks

### Acquisition order

Take locks only in this order. Locks on the same line are never held
together. A lock may be skipped, but never taken after a later one.

1. `Agent.ownershipMu`
2. `Session.flushMu`
3. `Agent.mu`
4. `Session.mu`
5. `mcp.Manager.mu`
6. `mcp.server.mu` | `mcp.peer.mu`
7. `jsonrpc.Reply.mu`
8. `jsonrpc.Conn.mu` | the batch collector's mutex in `Conn.handleBatch`

Leaves, never held while another lock is taken: the catalog cache mutex in
`cacheCatalog`, `calibration.mu`, `mcp.httpTransport.mu`, and outside these packages
`tools.ShellObserver.mu` and the Windows-only `winproc.Job.mu`.

Where each edge comes from:

| Edge | Where |
| --- | --- |
| `ownershipMu` → `Agent.mu` | `agent.go`: `Agent.renew`, `Agent.dropLocked`, `Agent.acquire`, `Agent.publishRestored` |
| `ownershipMu` → `flushMu` | `Agent.dropLocked` drains a save; `Agent.acquire` holds the live session's `flushMu` across `Store.Acquire`; `Agent.publishRestored` calls `Session.flush` |
| `flushMu` → `Agent.mu` | `Agent.acquire` removes a session that lost ownership while holding its `flushMu` |
| `flushMu` → `Session.mu` | `persist.go`: `Session.flush`; `session.go`: `Session.insert`; `Agent.acquire` → `Session.markLost` |
| `Agent.mu` → `Session.mu` | `Agent.setConfigOption` holds `Agent.mu` through `Session.setConfigOption`, so mode admission sees pending setup and queued prompts together |
| `Session.mu` → `Manager.mu` → `server.mu` | `turn.go`: `Session.request` → `Manager.Snapshot` |
| `Session.mu` → `Manager.mu` → `peer.mu` | `Session.Resume` → `Change.Commit` → `peer.fail` (`mcp/catalog.go`) |
| `Manager.mu` → `peer.mu` | `Manager.Recover` → `peer.err` |
| `Session.mu` → `Reply.mu` → `Conn.mu` | `Session.submit` and `Session.insert` settle replies under `Session.mu`; `Reply.settle` delivers under `Reply.mu`, and delivery calls `Conn.untrack` |

### What each lock guards

| Lock | Guards | Why it exists |
| --- | --- | --- |
| `Agent.ownershipMu` (`agent.go`) | Ownership transitions: lease renewal and its loss handling, `Store.Acquire` in `Agent.acquire`, publication and first save of a restored session | A renewal that finds a session lost must not interleave with a resume that reacquires it. Only background work takes it; turns never do |
| `Agent.mu` (`agent.go`) | `sessions`, `lost`, `operations`, `shuttingDown`, `selectionSaved` | The session map and the `sessionOperation` chain. Held briefly; never across I/O |
| `Session.flushMu` (`session.go`, `persist.go`) | Serialises store writes: `Session.flush` and the prompt's `Session.claimTurn` | Each save takes the latest state, so saves cannot reorder. Ownership changes take it to drain an in-flight save, so an old write cannot revive this process's owner ID after a takeover |
| `Session.mu` (`session.go`) | Every mutable `Session` field except those below: settings, `history`, `transcript`, `state`, `queue`, `worker`, `claimed`, `stopTurn`, `closed`, toolbox pins, roots, remembered shell decisions, `replaying`/`deferred`, and the persistence marks `saved`, `dirty`, `lost`, `saveWarning` | Transcript changes and their live update are one step, so a resume snapshot and live updates cannot reorder |
| `mcp.Manager.mu` (`mcp/catalog.go`) | `servers`, `revision`, `closed`, `resources`, each server's `recoveryAttempted`; `preparing.Add` happens under it | Configuration publication is atomic; `Close` cannot miss a preparation in progress |
| `mcp.server.mu` (`mcp/catalog.go`) | The server's `tools` | `server.refresh` revokes stale definitions under it, and `Tool.Call`'s admit check runs under it: the dispatch linearization point. A call is either admitted for the definition it was bound to or refused |
| `mcp.peer.mu` (`mcp/client.go`) | `next`, `calls`, `failure` | `peer.complete` publishes a response before unlocking, so a later failure cannot overtake a received reply |
| `mcp.httpTransport.mu` (`mcp/http.go`) | `version`, `sessionID`, `closed`; `jobs.Add` happens under it | `Close` waits for every admitted HTTP job |
| `jsonrpc.Conn.mu` (`jsonrpc/conn.go`) | `inflight`, `calls`, `lastCall` | Matching responses and `$/cancel_request` to requests. `Conn.deliver` and `Conn.cancelCall` race under it, so a delivered response wins |
| `jsonrpc.Reply.mu` (`jsonrpc/reply.go`) | `settled`, the stored result and the delivery binding | Only the first settlement counts, whether from work, the handler's return or a panic |
| catalog cache mutex (`catalog.go`: `cacheCatalog`) | The loaded catalog, its age, the load in flight | At most one load runs; callers wait on its channel outside the lock |
| `calibration.mu` (`calibration.go`) | Per-model byte/token ratios | Shared by every session of the process |

Not lock-guarded, by ownership instead:

- `Session.id`, `cwd`, `deps`, `catalog`, `ctx` and `cancel` are fixed once a
  session is published. `newSession`, `restoreSession` and the `session/new`
  goroutine in `Agent.newSession` write fields without locks only while the
  session is still private; publication under `Agent.mu` orders those writes.
- `Session.budget` belongs to the worker during one turn. Read-group workers
  only call `turnBudget.check`; `turnBudget.record` runs on the worker between
  rounds.
- `sessionOperation.err` is published by closing `done`. `prompt` is set on
  the read loop right after reservation; its readers are read-loop handlers
  or operation goroutines started later.
- `Agent.initialized` and `deps.forms` are atomics set by `initialize`.

### Where locks must not be held

| Must not hold | Across | How the code avoids it |
| --- | --- | --- |
| any agent lock | `Conn.Call`, `Conn.CallCancellable` | `Session.requestPermission` and `Session.askUser` set state, release `Session.mu`, then call |
| `Session.mu` | store I/O | `Session.flush` saves after unlocking; `Session.claimTurn` unlocks around `Store.Save` while `flushMu` keeps saves ordered |
| `Session.mu` | MCP `Prepare`, `Change.Discard`, `Manager.Close`, `Manager.Recover` | `Session.Resume` prepares before locking and defers `Discard`; `Change.Commit` under the lock only swaps pointers. `Session.loseLocked` closes MCP on a goroutine |
| `Session.mu` | `tools.New`, `Toolbox.Close`, `AGENTS.md` reads, model requests | `Session.Resume`, `Session.releaseToolbox`, `Session.refreshInstructions` and `Session.converse` do this work unlocked |
| `Session.mu` | writing a replay snapshot | `Session.replay` writes outside the lock; `Session.send` defers live updates meanwhile (see [Resume](#resume)) |
| `flushMu` or `Session.mu` | `Session.Close` | `Close` waits for the worker and admitted saves, which take both. `Agent.dropLocked` and `Agent.acquire` start it with `go` |
| `Agent.ownershipMu` | MCP setup, the catalog load, `restoreSession` | `Agent.open` takes the lock only in `Agent.acquire`, then waits for the catalog (up to `catalogTimeout` when none has loaded) and restores the private session; `Agent.resume` runs `Session.Resume` before `Agent.publishRestored`. A slow provider or server cannot delay lease renewal |
| `Manager.mu` | connecting or closing a server | `Manager.Prepare` and `Manager.Recover` connect unlocked; `Manager.closeResource` closes before locking |

Live updates are the deliberate exception: `Session.send` notifies under
`Session.mu`, so a resume snapshot and live state cannot reorder.
`Conn.Notify` blocks only when the 256-entry output queue is full, and the
writer goroutine takes no lock, so a slow client applies backpressure but
cannot deadlock the agent.

## Goroutines

Per connection:

| Goroutine | Started by | Ends |
| --- | --- | --- |
| Read loop: handlers in arrival order | `Conn.Serve`, on `main`'s goroutine | input ends or the signal context is cancelled |
| Line reader | `Conn.Serve` → `readLines` | input ends, or at its next line after `Serve` returns |
| Writer, the only stdout writer | `jsonrpc.NewConn` → `Conn.writeLoop` | `Conn.Close`, after flushing the queue |
| Batch response collector | `Conn.handleBatch`, one per batch | every request in the batch is answered |
| Catalog load | `agent.New` (`go a.catalog()`), then one per load or stale reload in `cacheCatalog` | the load finishes (bounded by `catalogTimeout`) |
| Lease renewal | `agent.New` → `Agent.renewLeases` | `Agent.ctx` is cancelled |
| Selection save, chained in request order | `Agent.rememberSelection` | its save finishes |
| `session/new`, `session/list` work | `Agent.newSession`, `Agent.listSessions`; tracked by `Agent.jobs` | reply settled |
| One per `sessionOperation` | `Agent.startOperation`; tracked by `Agent.jobs` | its work and `done` |
| Session closers | `Agent.Shutdown`, `Agent.dropLocked`, `Agent.acquire` | `Session.Close` returns |

Per session:

| Goroutine | Started by | Notes |
| --- | --- | --- |
| Worker | `Session.submit`, when the queue gains work and none runs | `Session.work` runs turns one at a time and exits when the queue is empty; `Session.worker` is closed then |
| Prompt withdrawal | `context.AfterFunc` in `Session.submit` | rejects a queued prompt whose request was cancelled |
| Async save | `Session.flushAsync`; tracked by `Session.saves` | `Session.Close` waits for it before the lease can be released |
| Read-group workers | `orderedReads` in `parallel_reads.go` | up to `ACPLAUSE_READ_CONCURRENCY` (at most 8) built-in reads against the worker's pinned toolbox. Outcomes are committed on the worker in call order; all join before return |
| Terminal pump | `Session.executeTool` in `terminal.go`, per observed shell call | publishes shell output under `Session.mu` and saves at tool start; joined before the tool finishes |
| MCP close on loss | `Session.loseLocked` | |

Per MCP server, all owned by the session's `mcp.Manager`:

| Goroutine | Started by |
| --- | --- |
| Parallel connects, at most four | `Manager.Prepare` |
| Reply sender for server-initiated requests | `newPeer` |
| `tools/list_changed` refresher | `connectServer` → `server.watch` |
| Stdio: process waiter, stdout reader, stdin writer, closer | `startStdio`, `stdioTransport.start` |
| HTTP: closer, one per POST (at most 64), one GET event stream | `httpTransport.start`, `httpTransport.launch`, `httpTransport.startGET` |
| `notifications/cancelled` after a caller stops waiting | `peer.callDispatch` |
| Retiring replaced or discarded servers | `Change.Commit`, `Change.Discard`, `Manager.Recover`, `Manager.Close` |

`context.AfterFunc` callbacks also run on their own goroutines: they are used
for cancellation links in `Agent.reserveOperation`, `Session.insert`,
`Session.executeToolCall`, `connectServer`, `Tool.Call`, and both transports.

## Replies and the insertion boundary

Handlers that wait return a `*jsonrpc.Reply` and settle it later
(`jsonrpc/reply.go`). The reply is bound before the handler runs, so work may
settle it first; only the first settlement counts. A request's context ends
when it is answered, on `$/cancel_request`, or when `Serve` returns.

| Method | Settled by |
| --- | --- |
| `initialize`, `session/set_config_option` | the handler's return, on the read loop |
| `session/new` | the goroutine in `Agent.newSession` |
| `session/list` | the job in `Agent.listSessions` |
| `session/resume` | the operation goroutine (`Agent.resumeSession`) |
| `session/close`, `session/delete` | the operation goroutine; terminal cleanup runs even if the reply was already rejected as cancelled |
| `session/prompt`, before submission | the handler's return when the session is unknown or lost; otherwise `Agent.startOperation`, when the prompt waited behind setup and was cancelled, setup failed, or the session is gone |
| `session/prompt`, after submission | the `Session`: `Session.submit` (closed, lost, no model), `Session.withdraw`, `Session.Cancel` (queued prompts), `Session.insert` on the worker |
| any handler that panics or a method that does not exist | `Conn` itself |

`session/cancel` is a notification. `Agent.cancelWaitingPrompts` only cancels
chained prompt contexts and never settles them, because a store commit racing
the cancellation may already have crossed insertion.

The **insertion boundary** is `Session.insert` → `Session.claimTurn`
(`persist.go`), on the worker, under `flushMu` and `Session.mu`.
`claimTurn` stages the user message and the running state in one
ownership-checked `Store.Save`; its successful commit is the boundary. Without
a store, or after a save failure that is not a loss, the in-memory fallback
crosses it too. Before the boundary, `insert` rejects: `errSessionLost`, `-32800`
Request cancelled, invalid images or an unusable model. After it, `insert`
resolves `messageId` under `Session.mu` before sending `user_message`, so for
a single request the response precedes the update. The turn's context
derives from `Session.ctx`, not from the request, so answering the request
does not stop the turn. `Session.runTurn` then sends exactly one idle
`state_update` under `Session.mu`, `cancelled` whenever the turn's context
ended.

## Session operations

`sessionOperation` (`lifecycle.go`) orders the session methods that do work
off the read loop. Handlers cannot block, so without it a prompt sent after
`session/resume` could run before the resume published the session, a later
resume could overtake an earlier one, and a close could miss a session that a
pending resume publishes afterwards.

- `Agent.reserveOperation` runs on the read loop, under `Agent.mu`. It links
  the operation to the session's current tail (`previous`), makes it the new
  tail and adds it to `Agent.jobs`. The operation's context also ends with
  `Agent.ctx`.
- `Agent.startOperation` waits on a goroutine for `previous.done`. If its own
  context ends first, it rejects the reply as cancelled and still waits for
  its predecessor, so cancellation cannot let a later operation overtake
  setup. Closing `done`, under `Agent.mu`, publishes `err` and removes the
  operation if it is still the tail.
- **Prompt.** With nothing pending, `Agent.prompt` submits straight to the
  session queue. Otherwise it joins the chain marked `prompt` and, once
  predecessors finish, takes the error of the nearest non-prompt
  predecessor: failed setup rejects the prompt before insertion, while an
  earlier cancelled prompt does not. Then it submits with the request's
  reply, which the session owns from then on.
- **Resume.** `Agent.resume`: `Agent.open`, `Session.Resume`, then
  `Agent.publishRestored` for a restored session. Replays never overlap,
  because operations on one session are serial.
- **Close and delete** interrupt: their reservation cancels every
  predecessor but still waits for each to finish. The handler first removes
  the session from the map and calls `Session.Cancel`, so later messages see
  it gone and the running turn stops at once. `close` answers not found
  unless a session was open or unfinished setup could still open one.
  `Agent.terminalSession` then catches a session that a cancelled resume
  published late, closes it and releases the lease; `delete` also removes the
  record.
- `session/set_config_option` refuses a mode change while any operation is
  pending, under `Agent.mu`.

## Resume

`Agent.open` calls `Agent.acquire`, which runs `Store.Acquire` and the loss
handling below under `ownershipMu`, and under the live session's `flushMu`
when there is one, so old saves finish before ownership changes. Loading the
catalog and restoring happen after both are released.

- **Continuously owned live session** (`Store.Acquire` reports it already
  owned and it is not lost): the same `Session` is reused and a running turn
  continues. `Session.Resume` builds the new toolbox and prepares MCP
  servers unlocked, then commits under `Session.mu`. A toolbox the worker has
  pinned is closed by `Session.releaseToolbox` once the worker lets go.
  `Change.Commit` keeps servers whose configuration is unchanged, with their
  bindings, and fails the connections of the others, which ends their calls
  with an "outcome may be unknown" error. The current model round keeps its
  own binding snapshot (`modelRound`), so it is never redirected to a new
  server. Definition changes from `tools/list_changed` are revoked separately
  by `server.refresh`; there a call already admitted runs on.
- **Replay.** Under `Session.mu`, `Session.Resume` copies the transcript plus
  the current state and sets `replaying`. `Session.replay` writes that
  snapshot without the lock, so a slow client cannot stall the read loop on
  it. Meanwhile `Session.send` appends live updates to `deferred`; `replay`
  then drains them under the lock until none remain. Transcript entries are
  replaced, never mutated in place, which makes the unlocked read safe.
  Without replay, the current state is sent under the lock.
- **Otherwise** the old session is marked lost, which cancels its turn,
  removed from the map and closed on a goroutine. A new session is restored
  from the store, repairing a turn its last process left unfinished. It stays
  private until `Agent.publishRestored` saves it under `ownershipMu` and
  publishes it under `Agent.mu`, unless the request was cancelled, the agent
  is shutting down or the session was lost meanwhile. On any failure
  `Agent.resume` closes the restored session and releases its lease.

## Lease loss

Loss is detected in three places:

- `Agent.renew`, every `store.LeaseRenewal`, under `ownershipMu`. For each
  lost ID, `Agent.dropLocked` removes the session, records it in `lost` so
  later requests fail with `errSessionLost`, and cancels the operation chain
  captured at the check. A resume reserved after the check is left alone so it
  can restore the session. It then marks the session lost, drains `flushMu`
  and closes it on a goroutine.
- `Session.flush` or `Session.claimTurn` getting `store.ErrLost`, under
  `flushMu` and `Session.mu`. `Session.loseLocked` marks the session lost; it
  stays in the map until the next renewal drops it.
- `Agent.acquire`, when the live session was not continuously owned.

`Session.loseLocked` sets `lost`, cancels `Session.ctx` and closes MCP servers
on a goroutine. A running turn's context is cancelled, so the turn ends with
an idle `cancelled` state; save-failure and turn time-limit notices are
suppressed. `Session.flush` stops saving. `Session.submit`, `Session.insert`
and `Session.updatePlan` refuse with `errSessionLost`. A prompt whose
`claimTurn` committed before the loss is acknowledged and then cancelled.
`Agent.release` skips lost sessions, since the lease belongs to the other
process.

## Shutdown

`cmd/acplause` calls `Agent.Shutdown` after `Conn.Serve` returns, then
`Conn.Close`. `Serve` returning cancels every pending request context, so
queued prompts are withdrawn and waiting operations rejected. `Shutdown`:

1. waits for the last queued model-selection save, which uses `Agent.ctx`;
2. cancels `Agent.ctx`, the parent of every session context and operation;
3. sets `shuttingDown` and empties the map under `Agent.mu`, so a
   `session/new` or restored resume still in flight discards its session
   instead of publishing it;
4. closes every session and releases its lease in parallel. `Session.Close`
   waits for the worker, so a running turn's final idle `cancelled` update is
   queued before `Conn.Close` flushes the writer. The turn's last
   `Session.flush` saves without cancellation;
5. waits for `Agent.jobs`: `session/new`, `session/list` and session
   operations.

The store closes after `Shutdown` returns.
