# ADR 0034: Reserve wire cancellation for pending permission requests

- Status: accepted
- Date: 2026-11-26
- Issue: [#221](https://github.com/anesthetised/acplause/issues/221)
- Supersedes decision 7 of [ADR 0003](0003-permissions.md).
- Protocol: [ACP beta.4 cancellation](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/cancellation.mdx) and [closing active sessions](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#closing-active-sessions).

## Context

When foreground work is cancelled, beta.4 requires the agent to cancel its
pending permission requests. Close also requires the session's cancellations
and final foreground updates to precede its reply. A full ordinary output
queue can drop a best-effort notification. Waiting on that queue to cancel
would hold up the worker that close needs to join.

## Decision

1. Each connection keeps 256 ordinary output slots and a separate FIFO with
   256 cancellation slots. A session worker first reserves one cancellation
   slot, then obtains ordinary capacity for the permission request. Both waits
   observe the request context and connection shutdown. Losing admission
   releases acquired capacity and emits nothing.
2. Register the response waiter when the request enters the ordinary FIFO.
   Response delivery and cancellation compete to remove that waiter. A
   response releases the reservation. When cancellation wins, the worker
   transfers its reservation to a `$/cancel_request` with the original
   request ID before returning. Late answers are ignored. The session
   rechecks cancellation before executing a tool or recording a shell
   decision under [ADR 0029](0029-session-remembered-permissions.md).
3. One enqueue order spans both FIFOs. The single stdout writer consumes
   lines in that order, including during graceful close. A write failure
   closes admission, wakes waiters and discards undeliverable output.
4. A close operation waits for its predecessor, worker, resource cleanup and
   lease release. If the close request itself was cancelled, it answers
   `-32800` after the same cleanup. Forms keep their best-effort cancellation
   through ordinary capacity under [ADR 0011](0011-structured-user-questions.md).

## Consequences

A stalled peer can delay permission admission across sessions once the 256
reservations are occupied. It can also delay physical delivery and graceful
shutdown until stdout drains. Cancellation of an admitted permission can
always enter its FIFO while the connection is open. Both payload buffers
and their order bookkeeping have fixed bounds.
