# ADR 0011: Ask structured questions through ACP form elicitation

- Status: accepted; outbound cancellation implemented in #29, form elicitation pending #30
- Date: 2026-10-19
- Issues: [#29](https://github.com/anesthetised/acplause/issues/29), [#30](https://github.com/anesthetised/acplause/issues/30)
- Scope change: No existing decision; outbound cancellation for questions is opt-in and preserves ADR 0003 permission behavior
- Baseline: acplause `d9c99f1`; ACP `2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`

## Context

T3 renders elicitation/create as interactive question cards. acplause cannot currently negotiate elicitation or expose a question tool to its model.

## Decisions

1. Read and retain capabilities.elicitation.form from initialize. Only an explicit non-null form object enables the tool. Omitted, null, empty elicitation objects and unknown variants do not. Missing support leaves the model able to ask ordinary textual questions; do not fabricate structured answers.

2. Expose an agent-owned ask_user tool only when form support is advertised. Keep it out of filesystem Toolbox. Arguments are {message, questions:[{id,title,question,options?}]}; one to three questions, unique nonempty IDs (at most 64 characters), title up to 120 characters, question/message up to 2000 characters, and at most ten unique nonempty option strings of 200 characters. Each question maps to a required string property; options map to enum. No arbitrary JSON Schema supplied by the model, numeric coercion, multiselect, secrets, URL mode or nested forms in this slice.

3. Send elicitation/create with mode:'form', sessionId, toolCallId, message and requestedSchema using the pinned flattened scope. Only the session worker blocks. Report the tool pending and requires_action while foreground work waits, then running before continuing; every model call still gets a tool result.

4. Accept only action:'accept' with a complete object of string answers conforming to the advertised fields/enums. Unknown keys, unknown actions, invalid types and missing required answers are failed tool results, not permissions. Decline/cancel become explicit failed tool results the model can handle; a cancelled turn instead ends idle(cancelled). Never execute an action merely because a form was accepted.

5. Add an explicit outbound cancellable-call entry point to jsonrpc.Conn that retains the request ID and emits $/cancel_request when local cancellation makes a sent question irrelevant. Keep ordinary Call behavior unchanged for permissions. Request enqueue must precede cancellation enqueue through the single writer; do not send cancel for an unqueued request. Cleanup must not wait for a user or a blocked output queue; cancelled/closed late replies cannot restart a turn.

6. Keep live question state connection-local. Same-connection resume does not reissue a question. Process loss uses existing unfinished-tool recovery; there is no durable approval or automatic replay of a question. Test close/delete, lease loss and cancel/answer races.

7. Direct agent form elicitation is the scope. Relaying server-initiated MCP elicitation requires a separate future design and does not change ADR 0007's MCP client capability advertisement.

## Consequences

A deliberately small string/enum surface matches T3's current rendering without building a general forms framework. The user can answer normal questions in-place, while permission and auth contracts remain distinct.

## Sources and execution

Wire shapes come only from the pinned schema and these pinned protocol pages:

- [initialization](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/initialization.mdx)
- [elicitation](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/elicitation.mdx)
- [cancellation](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/cancellation.mdx)
- [prompt-lifecycle](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/prompt-lifecycle.mdx)

T3 compatibility evidence: checkout `94285424c`, `packages/provider-acp/src/server/{adapter,runtimeModel,AcpSessionRuntime}.ts` and `docs/user/providers-acp.md`. This is client evidence, not a source of ACP wire shapes.

Execution plan: [questions](../../tasks/t3/questions.md). The accepted design is
implemented in separate slices: #29 adds opt-in transport cancellation without
activating a client request; #30 supplies form elicitation and its protocol e2e
coverage. Actual T3 acceptance remains tracked in #38.

