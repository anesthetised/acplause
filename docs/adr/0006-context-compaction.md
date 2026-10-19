# ADR 0006: Compact the model history before its context window fills

- Status: accepted
- Date: 2026-10-19
- Issue: #11
- Source: ACP schema at `3be9cfc` (`testdata/schema/v2/VERSION`),
  `CompactionUpdate`, `protocol/v2/prompt-lifecycle.mdx#session-compaction`;
  [OpenRouter parameters](https://openrouter.ai/docs/api/reference/parameters)

## Context

The model history grows after every prompt and tool round. A provider can
eventually reject it for exceeding the context window. The editor's replay
transcript is separate, so reducing model context need not erase what the
user saw. No tokenizer is shared by all supported models.

## Decisions

1. **Check before every model request.** Use the selected model's advertised
   context length. Budget 75% for input and reserve 25% for output; send
   `max_tokens` capped at 8192 and the catalog's nonzero
   `top_provider.max_completion_tokens` ceiling. Count serialized message and
   tool-definition bytes as tokens, plus framing overhead. This deliberately conservative
   estimate includes arguments and reasoning, works without a tokenizer
   dependency, and can compact earlier than necessary. It is not an exact
   provider token count. An unknown context length leaves existing behavior
   unchanged; do not guess a window or choose another model.
2. **Trim tool output first, oldest first.** When over budget, shorten tool
   results longer than 2 KiB, keeping their beginning and end with an explicit
   omission marker. Stop once the request fits. Calls, arguments, IDs and
   matching results remain present. Even the newest result may be shortened
   when needed, so reading a large file need not overflow the window.
3. **Summarize only if trimming is insufficient.** Use the same selected
   model, without tools, to summarize older messages. Preserve goals, user
   constraints, decisions, file changes, tool outcomes and pending work.
   Keep the system prompt, latest user prompt and most recent complete
   assistant/tool exchange in the active turn verbatim (apart from result
   trimming). Completed earlier turns and older exchanges within the current
   turn may be summarized. Never split
   a tool call from its results. Store the summary as an assistant message,
   not as system instructions. Future compactions summarize earlier summaries.
   Historical messages are supplied as JSON data, with reasoning fields
   excluded so hidden reasoning cannot leak into the displayable summary.
4. **Bound summary requests too.** Consume old history in complete exchanges
   that fit the same input budget, folding each chunk into a rolling summary.
   Summary calls count against `ACPLAUSE_MAX_ROUNDS`, reserving a request
   for the actual answer. Require a nonempty, normally finished text summary
   with no tool calls, and verify the final conversation fits before replacing
   history. An oversized indivisible exchange, system prompt, latest user
   prompt, or unusable summary fails explicitly rather than silently dropping
   instructions or sending a known oversized request.
5. **Report and replay compaction.** Send `compaction_update` with a stable ID
   and `in_progress`, then `completed` with the full summary, or `failed` /
   `cancelled`. The first update fixes its transcript position. Replay sends
   its latest snapshot in that position, together with the original complete
   conversation. Summary generation does not stream ordinary answer chunks.
   Foreground state stays running and ends with the usual single idle update.
6. **Persist atomically.** Save the replaced model-history suffix and the
   completed compaction snapshot in the same ownership-checked transaction.
   Remove obsolete history rows; retain original replay entries. Interrupted
   compactions restore as failed, using the unchanged pre-summary history.
   Failed or cancelled summaries do not replace history; completed result
   trimming is retained. Storage failures follow ADR 0004's in-memory fallback.
7. **Account for work.** Include summary request cost in cumulative usage.
   Cancellation aborts summary generation through the turn's context. Model
   and directory changes apply when preparing the next normal request;
   one compaction's summary calls use the model selected when it began.

## Consequences

- Long conversations keep working with bounded model context, at the cost of
  extra requests, latency and imperfect model-written summaries.
- Replay and storage size still grow until the session is deleted; this is
  context management, not a data-retention policy.
- A conservative byte estimate sacrifices some usable context for portability.
  Models without a published window and provider-specific token overhead
  remain limitations.
- Silent deletion of old turns loses decisions; a fixed message count ignores
  large tool outputs. Both were rejected in favor of budgeted summaries.
