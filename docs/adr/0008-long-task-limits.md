# ADR 0008: Allow long agent tasks with explicit unlimited settings

- Status: accepted
- Date: 2026-10-19
- Supersedes: ADR 0007 decision 12's default tool-call deadline only

## Context

Heavy coding tasks can exceed 25 model requests, two-minute shell commands,
and ten-minute MCP calls, especially when an MCP tool waits for a delegated
agent. Previously only `ACPLAUSE_MCP_TIMEOUT` accepted zero as unlimited;
round and bash limits rejected it, and their internal zero values selected
defaults. Compaction also consumes the model request budget.

## Decisions

1. **Keep finite defaults with room for long tasks.** Allow 200 model
   requests per turn, 30 minutes per bash command, and one hour per MCP tool
   call. The request budget accommodates iterative implementation and
   compaction; shell commands have time for builds and tests; external calls
   have more time for delegated work.
2. **Use zero consistently.** Explicit `0` disables the corresponding limit
   for `ACPLAUSE_MAX_ROUNDS`, `ACPLAUSE_BASH_TIMEOUT`, and
   `ACPLAUSE_MCP_TIMEOUT`. Unset or empty values select defaults. Reject
   negative values and malformed inputs. Internal optional settings use nil
   for defaults and a pointer to zero for unlimited.
3. **Include compaction in the same policy.** Summary requests consume a
   finite turn budget and reserve a request for the actual answer, as in
   ADR 0006. With an unlimited turn budget, summaries impose no request cap;
   context-size checks and summary validation still apply.
4. **Preserve cancellation and other deadlines.** Turning off a tool's
   deadline does not detach it from the turn's context. MCP setup, discovery,
   refresh and cleanup retain their existing deadlines. Tool deadlines still
   start after permission; MCP cancellation and no-replay behavior stay as
   specified in ADR 0007.

## Consequences

- Long tasks have more headroom without requiring environment overrides.
- A stuck operation can take longer to hit its default limit. Unlimited
  settings require cancellation to stop work that does not finish itself;
  the model request count then provides no automatic cost bound.
- The ACP wire schema, stored sessions and dependencies do not change.
