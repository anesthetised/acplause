# ADR 0032: Reject JSON-RPC batches on ACP stdio

- Status: accepted
- Date: 2026-11-26
- Issue: [#220](https://github.com/anesthetised/acplause/issues/220)
- Supersedes: [ADR 0001](0001-scope.md), decision 5 only
- Source: [ACP v2 beta.4 transport at the pinned commit][transport]

## Context

acplause followed an earlier ACP draft that accepted JSON-RPC batches. It
dispatched entries in order and collected replies until the last request
settled. A prompt could emit its user message while its acknowledgement
was still waiting for another batch member. Notification-only batches ran
silently. The e2e reader split stdout arrays before checking the schema,
so individually valid replies could hide an invalid frame.

Keeping batch support would preserve compatibility with clients that group
messages, but beta.4 requires the receiver to reject each such frame. [Pinned
transport rules][transport]

## Decisions

1. **Reject the entire frame before dispatch.** Every syntactically valid
   top-level array within the existing line limit gets one JSON-RPC error
   object with `jsonrpc: "2.0"`, an explicit `id: null` and
   `error.code: -32600` (Invalid request). This includes empty, singleton,
   notification-only, response-only and mixed arrays. No member invokes a
   handler, cancels a request or settles a pending call. Continue reading the
   same connection. Malformed JSON still gets a parse error (`-32700`).
   Arrays inside an object's payload follow that method's schema.
2. **Preserve ordering for individual messages.** Admit handlers on the read
   loop in arrival order. Bind deferred replies before invoking the handler
   and enqueue the first settlement through the single writer. A prompt
   cancelled before insertion gets `-32800`. After insertion, its successful
   `messageId` reply precedes its `user_message` echo and subsequent turn
   updates. A following `session/cancel` observes the admitted prompt. A
   cancelled turn ends with one idle update carrying `stopReason: "cancelled"`.
3. **Validate each stdout line against the pinned root schema.** Apply the
   existing method-specific payload and error checks after validating the
   complete frame. Keep the physical line number for ordering assertions.

## Consequences

Clients that used batches must send one JSON-RPC object per line and
correlate replies by ID. They can still send the next request or cancellation
while a deferred reply is pending.

The transport needs one reply path. Rejecting an array requires no per-entry
decoding, response collector or goroutine waiting for deferred replies.
Transport and stdio tests cover rejection, continued service and ordinary
prompt ordering. The reader regression uses both an empty array and an array
of otherwise valid objects.

[transport]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/transports.mdx#no-batches
