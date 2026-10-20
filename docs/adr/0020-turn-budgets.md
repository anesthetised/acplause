# ADR 0020: Add opt-in budgets for each accepted turn

- Status: accepted; usage availability implemented in #81; enforcement pending #82/#83
- Date: 2026-10-20
- Issues: [#81](https://github.com/anesthetised/acplause/issues/81), [#82](https://github.com/anesthetised/acplause/issues/82), [#83](https://github.com/anesthetised/acplause/issues/83)
- Extends: [ADR 0008](0008-long-task-limits.md); existing defaults remain in force

## Context

Request counts and individual tool deadlines do not bound elapsed turn time or
reported expenditure. Providers can omit usage fields, and a missing USD cost
currently has the same numeric zero as a free response. Enforcement must retain
that distinction, include compaction and preserve the prompt insertion and
cancellation invariants.

## Decisions

1. **Opt-in settings, parsed at startup.** Unset, empty and zero select unlimited
   for each new setting. Positive values enable only that metric:

   | Setting | Value |
   | --- | --- |
   | `ACPLAUSE_TURN_TIMEOUT` | Nonnegative Go duration, such as `15m` |
   | `ACPLAUSE_MAX_TURN_TOKENS` | Nonnegative base-10 integer fitting `uint64` |
   | `ACPLAUSE_MAX_TURN_COST_USD` | Finite nonnegative base-10 decimal, optionally with a decimal exponent |

   Reject malformed, negative, overflow and nonfinite values before serving.
   Cost uses `float64`; reject a positive value that cannot be represented as
   positive finite USD rather than silently disabling the limit. Hexadecimal
   cost notation is not decimal. Configuration errors name the setting and
   expected type without echoing its potentially credential-bearing input.
   Existing request/tool defaults and MCP setup/discovery/cleanup deadlines
   remain those of ADR 0008. No price lookup, model fallback or new dependency.

2. **One allowance per accepted prompt.** Start fresh counters and, when
   enabled, a derived work deadline immediately after durable prompt insertion
   succeeds. Queue waiting, session setup and database insertion waiting do not
   consume the allowance. Ordinary turns and manual `/compact` share this
   lifecycle; automatic and manual summary requests share their turn's counters.
   No per-session billing cap, persistent budget or database migration. Existing
   cumulative session cost reporting remains separate.

3. **Deadline and cancellation.** Turn time includes model streaming, retries,
   tools, permission/form waiting and compaction. Cancellation reaches the
   active operation through its existing context; retain permission `Call`,
   form `CallCancellable` and MCP no-replay behavior. Dispose of the timer on
   every exit. A cancelled work context always ends with exactly one
   `state_update {state: "idle", stopReason: "cancelled"}`, with no error.
   Distinguish the turn deadline with a cancellation cause. On an actual turn
   timeout, send one existing `notice` with warning severity, a concise title
   such as "Turn time limit reached", and an explanation that the configured
   turn deadline elapsed. Do not claim the user cancelled it. At finalization,
   user/parent cancellation, close/delete and ownership loss take priority over
   a racing deadline; suppress the timeout notice when the outer turn context
   is cancelled. Notices remain live metadata, outside conversation replay.

4. **Reported accounting only.** Preserve availability independently for input
   tokens, output tokens and USD cost. Absent/null values are unavailable;
   explicit zero counts and free (`cost: 0`) responses are available. Token
   accounting requires both input and output counts and their checked sum;
   count that sum once. Do not add cached/reasoning subsets again, substitute
   `total_tokens` for missing counts, or use the context byte estimator as a
   billing tokenizer. Require nonnegative finite cost, checked token sums and
   checked cumulative counters; invalid/overflow accounting is unusable for
   enforcement. Malformed JSON/types and negative integer token fields retain
   the client's existing protocol-error path, without provider raw bodies or
   credential values in errors. Availability metadata is internal, not ACP or
   persistence data. Legacy numeric fields remain usable by current consumers;
   programmatically constructed usage must explicitly mark available fields
   before enforcement can trust it.

5. **Threshold and missing-data semantics.** Record accepted usage once per
   completed request, before validating a summary or deciding whether its
   history replacement can be installed. Invalid or abandoned summaries still
   consumed that usage. After recording and before every further request,
   approval or tool execution, stop when any enabled metric is **greater than
   or equal to** its threshold. This includes a normally finished response
   with no remaining work: retain its assistant output but report the reached
   limit consistently. A configured metric with unavailable/unusable accounting
   stops further work explicitly as accounting unavailable. A disabled metric
   does not block a provider that omits it. Do not guess or silently treat
   unknown spend as zero. Cancellation takes priority over a budget error.

6. **Stop without side effects or invalid history.** A threshold or accounting
   failure after insertion ends with the existing `idle(error)` state and a
   clear error identifying the metric/reason. Use the existing internal-error
   representation; invent no ACP stop reason or wire field. Retain assistant
   output and append a matching failed tool-history result for every returned
   call that will not execute, including sibling calls. Start no approval,
   execution or next request after the stop. Keep already committed results and
   ownership checks. Incomplete compaction never replaces history; its existing
   lifecycle reports failure/cancellation, while accepted usage stays counted.

7. **Groundwork and activation.** #81 supplies this ADR and presence-aware
   decoding/checked accessors only. It introduces no active allowance or parsed
   environment setting. #82 activates the post-insertion deadline, and #83
   activates token/USD accounting shared by normal and summary paths. Each
   activation updates README and this status after lifecycle, race and
   schema-validating protocol tests. ACP shapes remain pinned to the existing
   schema; these tests do not establish real-T3 acceptance under #38/#25.

## Consequences

All new limits default off, preserving long tasks. A provider request can
overshoot token/USD limits by one completed response; float USD accounting also
retains provider/float precision limits. These are reported-usage stopping
policies, not prepaid or hard provider billing ceilings. An in-flight or failed
request can incur unreported charges. A configured metric may halt a provider
that cannot supply valid accounting, while explicit zeros remain useful for
free responses. No automatic pricing discovery, budget persistence or model
substitution is introduced.
