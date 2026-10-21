# Configuration

[Documentation](README.md) · [Project overview](../README.md)

| Variable                | Default                         | Description                           |
| ----------------------- | ------------------------------- | ------------------------------------- |
| `ACPLAUSE_API_KEY`      |                                 | API key; unset sends no `Authorization` |
| `ACPLAUSE_BASE_URL`     | `https://openrouter.ai/api/v1`  | any OpenAI-compatible API root        |
| `ACPLAUSE_READ_CONCURRENCY` | `1`                         | concurrent built-in reads, integer `1..8` |
| `ACPLAUSE_MAX_ROUNDS`   | `200`                           | model requests per turn; `0` disables |
| `ACPLAUSE_BASH_TIMEOUT` | `30m`                           | bash tool timeout; `0` disables       |
| `ACPLAUSE_MCP_TIMEOUT`  | `1h`                            | MCP call timeout; `0` disables        |
| `ACPLAUSE_TURN_TIMEOUT` | `0`                             | accepted turn timeout; `0` disables   |
| `ACPLAUSE_MAX_TURN_TOKENS` | `0`                          | reported input+output tokens per turn; `0` disables |
| `ACPLAUSE_MAX_TURN_COST_USD` | `0`                        | reported USD per turn; `0` disables |
| `ACPLAUSE_DATA_DIR`    | `~/.acplause`                    | session database directory            |
| `ACPLAUSE_LOG_LEVEL`    | `info`                          | logs go to stderr                     |

Point your ACP v2 client at the `acplause` binary with these variables set.
Use [setup diagnostics](troubleshooting.md) to check the configuration.
See [parallel reads](tools.md#parallel-reads) for the read concurrency policy
and [model selection](usage.md#model-and-reasoning-selection) for session settings.

The key is sent only to `ACPLAUSE_BASE_URL`, as `Authorization: Bearer`.
Redirects are not followed: a provider that redirects fails until the final API
root is configured. Commands the bash tool runs inherit the environment without
`ACPLAUSE_API_KEY`. This prevents accidental exposure, not a deliberate read:
commands run as the same user. See [ADR 0022](adr/0022-provider-key-exposure.md).

## Request and tool limits

The defaults allow sustained tool work, long builds and tests, and delegated
MCP tasks. An unset or empty variable selects its default; explicit `0`
disables only that limit. Negative values are rejected. Cancellation still
stops a turn and its active tools when limits are disabled. MCP setup,
discovery and cleanup keep their separate deadlines. Summary requests also
count toward `ACPLAUSE_MAX_ROUNDS` when it is enabled. See
[ADR 0008](adr/0008-long-task-limits.md).

## Turn timeout

`ACPLAUSE_TURN_TIMEOUT` accepts a nonnegative Go duration such as `15m`.
It starts after prompt insertion, excluding queue, setup and insertion waiting,
and resets for the next accepted prompt. It covers model requests, built-in/MCP
tools, permission/form waiting and automatic/manual compaction. Expiry cancels
active work and ends once with `idle(cancelled)`, preceded by a live warning
notice explaining the turn deadline. User/session cancellation takes priority
and does not produce that timeout notice. Notices are not conversation replay.

## Token and cost budgets

`ACPLAUSE_MAX_TURN_TOKENS` accepts a nonnegative base-10 `uint64` count;
`ACPLAUSE_MAX_TURN_COST_USD` accepts a finite nonnegative decimal (including
decimal exponent notation). Unset, empty and `0` disable each limit independently.
Invalid, negative, overflow, nonfinite and unrepresentably small positive USD
settings fail startup without echoing their input.

These counters reset after prompt insertion and include ordinary and
automatic/manual summary responses. Tokens count reported input+output once,
including their reasoning/cached subsets rather than adding those again.
Explicit zero tokens or free cost are valid. An enabled metric requires its
accounting fields; missing, null, invalid or overflowing accounting stops work
with an explicit error. Disabled metrics do not require those fields.

At or above either cap, the turn ends with `idle(error)`, even for a final answer
that needs no more work. Assistant output is retained; returned tool calls get
failed results without approvals or execution. No further request starts.
Cancelled contexts still end with `idle(cancelled)`. Summary usage counts even
if validation fails or the summary cannot be installed; unfinished compaction
never replaces history. Cumulative session cost reporting remains separate.

A request can overshoot token/USD caps by one response. These limits use
reported usage, with provider and floating-point precision limits; they are not
prepaid or hard billing ceilings, and failed requests can incur unreported
charges. There is no pricing lookup, estimate-based token charge, model switch
or persistent allowance. See [ADR 0020](adr/0020-turn-budgets.md).
