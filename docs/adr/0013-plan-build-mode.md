# ADR 0013: Expose a restrictive Plan mode through session configuration

- Status: accepted; implemented with schema-validated tests; real-T3 validation pending (#38)
- Date: 2026-10-19
- Issues: [#33](https://github.com/anesthetised/acplause/issues/33), [#34](https://github.com/anesthetised/acplause/issues/34)
- Scope change: Extends session config; retains ADR 0003 client-owned approval policy
- Baseline: acplause `d9c99f1`; ACP `2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`

## Context

T3's Plan/Build toggle maps to ACP config options in category mode. acplause exposes only model and reasoning effort, and a textual instruction alone cannot ensure that planning tools avoid side effects.

## Decisions

1. Add configId:'mode', type:'select', category:'mode', values 'build' and 'plan'; default new sessions to build. Return it in new/resume and the full set_config_option response. Do not add v1 session/set_mode or send redundant config_option_update for client-originated changes (ADR 0001).

2. Persist mode on the session using one appended SQLite migration with default 'build'. Old sessions load build. Do not include mode in globally remembered model/effort preferences. Reject invalid values without mutation.

3. Make mode changes only while the session is idle with no admitted queued prompts or setup operation that could race activation. Reject a busy change with -32602 and a clear cancel/wait-and-retry message. Same-value requests are idempotent. This bounds semantics for already-issued permissions and model calls; an in-flight call never becomes retrospectively forbidden or newly permitted.

4. Plan mode offers read_file, list_dir, search_code and agent-owned ask_user/update_plan when available. It omits and rejects write_file, edit, bash and all external MCP tool calls, including readOnlyHint. Built-in reads remain os.Root confined. A stale/hallucinated forbidden call fails before permission or execution and still receives a tool result.

5. Construct model-round tools and invocation guards from the same mode snapshot. Keep MCP setup/discovery and existing session lifecycle independent of tool permission; a configured server can stay connected in Plan mode. No SQL/network/filesystem side effects are promised absent outside the allowed agent tool policy.

6. Add Plan instructions to the rebuilt system prompt: inspect and propose; do not claim changes were applied; use questions when needed. Switching mode rebuilds this section without losing AGENTS.md, roots or model instructions. Build restores normal tools and existing client permission requests; never introduce brave_mode or auto-allow state.

7. T3 uses value 'plan' and restores a build configuration on returning to Build. Verify both directions against the actual client. Busy-transition refusal is part of this release's documented limitation, not an invitation to weaken the guard.

## Consequences

Plan mode is deliberately restrictive: even read-only-looking MCP tools and shell inspection are unavailable. This avoids an unsafe shell parser or trusting external annotations. It is an execution policy, not an OS sandbox and not a permission preference.

## Sources and execution

Wire shapes come only from the pinned schema and these pinned protocol pages:

- [session-config-options](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/session-config-options.mdx)
- [tool-calls](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/tool-calls.mdx)
- [prompt-lifecycle](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/prompt-lifecycle.mdx)

T3 compatibility evidence: checkout `94285424c`, `packages/provider-acp/src/server/{adapter,runtimeModel,AcpSessionRuntime}.ts` and `docs/user/providers-acp.md`. This is client evidence, not a source of ACP wire shapes.

Execution plan: [modes](../../tasks/t3/modes.md). Issues #33 and #34 implement this contract. Automated tests cover both toggle directions, restart/resume, stale forbidden calls, queued work, pending setup and permission. The actual T3 client gate remains pending under #38; fake-client tests do not satisfy it.

