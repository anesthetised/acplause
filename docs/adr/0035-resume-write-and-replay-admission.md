# ADR 0035: Bound replay and activate cold sessions on write completion

- Status: accepted
- Date: 2026-11-26
- Issue: [#219](https://github.com/anesthetised/acplause/issues/219)
- Protocol: [ACP v2 beta.4 session setup](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#replay-complete)

## Context

A client can read a complete resume response while the agent's stdout
`Write` is still running. The same write can fail or report a short count.
A running session can also produce live updates while a cursor resume is
waiting for storage, MCP setup or writer capacity.

## Decision

1. While a cold resume response is queued, prompt and config requests
   receive `-32002` with the session ID. The writer's pre-write callback
   opens admission for deferred requests against that exact generation.
   A full successful write releases them in arrival order. Request and
   session cancellations still withdraw uninserted prompts. Keyed attempts
   share the insertion outcome under [ADR 0033](0033-prompt-idempotency-keys.md).
   A failed write or generation invalidation prevents execution.
2. Register a supported cursor at receipt before session lookup. Order live
   updates, permission requests and elicitations against this registration
   when they enter the ordinary FIFO. Each admitted permission retains its
   original ID and cancellation reservation under [ADR 0034](0034-permission-request-cancellation.md).
3. Reserve one ordinary slot per accepted replay, with a maximum of 16
   such slots per connection. Decline a cursor with an internal error when
   capacity is unavailable. Hold at most 64 live frames or 16 MiB per
   replay and 256 frames or 64 MiB across the connection. Count encoded
   bytes including NDJSON newlines. Queued completion items continue to
   consume their credits until the writer takes them.
4. Complete replay with one writer item containing the marker and owed
   live frames, including current state at its capture position. Emit a
   separate NDJSON line per frame. On overflow, commit an error for the
   affected request and every original live frame, including the trigger.
   The trigger occupies the reserved ordinary item directly, even when it
   alone exceeds the byte limit. The writer uses its existing encoded
   bytes. Other sessions continue. Stale history admissions fail.
5. Use the acplause-specific retryable error `1001` when an earlier cursor
   or a same-session operation still owes conflicting output. Once the last
   update enters the output stream, a later cursor can wait behind the
   existing lifecycle operation for cleanup and lease release.

## Consequences

Speculative requests during a write can succeed when the write completes.
A client whose replay overflows receives its live stream and can request
history again. Reservation pressure can decline a cursor on an otherwise
idle session. The additional replay budgets are independent of retained
history and of ordinary output backpressure.
