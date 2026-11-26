# ACP v2 conformance matrix (non-auth)

This matrix tracks [#157](https://github.com/anesthetised/acplause/issues/157)
against the ACP `2.0.0-beta.4` [schema](../../testdata/schema/v2/schema.json)
at commit `840484ff`. See the [schema pin](../../testdata/schema/v2/VERSION)
and the beta.4 audit below. The [T3 smoke record](t3-first-class-smoke.md)
tracks actual client rendering.

The e2e harness (`e2e/`) validates each complete stdout line against the
pinned root schema, then checks the response, error or method parameters
against their specific definitions. The test references point to relevant
files. The matrix was first compiled on 2026-10-22 from the schema's
`x-method` values, `SessionUpdate` variants and the Go types in `internal/acp`.

Legend: **Covered** = implemented and tested; **Deferred #N** = not
implemented, parked by the maintainer (needs a design decision first);
**Excluded** = out of scope by ADR.

## Methods

| Method | Direction | Status | Evidence |
| --- | --- | --- | --- |
| `initialize` | client→agent | Covered | `e2e/protocol_test.go`, `internal/acp/acp_test.go` |
| `session/new` | client→agent | Covered | `e2e/sessions_test.go`, `internal/agent/methods_test.go` |
| `session/prompt` | client→agent | Covered | `e2e/lifecycle_test.go`, `e2e/prompt_images_test.go`, `e2e/prompt_retries_test.go` |
| `session/cancel` | client→agent | Covered | `e2e/lifecycle_test.go`, `internal/agent/queue_test.go` |
| `session/list` | client→agent | Covered | `e2e/store_test.go` |
| `session/resume` | client→agent | Covered | `e2e/replay_unix_test.go`, `internal/agent/resume_setup_test.go` |
| `session/close` | client→agent | Covered | `e2e/sessions_test.go`, `e2e/store_retake_test.go` |
| `session/delete` | client→agent | Covered | `e2e/delete_test.go` |
| `session/set_config_option` | client→agent | Covered (select options only) | `e2e/selection_test.go`, `e2e/mode_test.go`; boolean options Deferred #143 |
| `session/update` | agent→client | Covered (see variants) | `e2e/` replay and lifecycle tests |
| `session/request_permission` | agent→client | Covered | `e2e/permission_test.go`, `e2e/remembered_permission_test.go`; all four option kinds for `bash` (ADR 0029), once-only elsewhere |
| `elicitation/create` | agent→client | Covered for tool-call scope | `e2e/elicitation_test.go`, `e2e/form_constraints_test.go`; session/request scope Deferred #139 |
| `elicitation/complete` | agent→client | Deferred #140 | no non-auth producer |
| `$/cancel_request` | both | Covered | `internal/jsonrpc/cancellable_test.go`, `internal/jsonrpc/permission_test.go`, `internal/jsonrpc/conn_test.go` |
| `auth/login`, `auth/logout` | client→agent | Excluded | ADR 0023 |
| [JSON-RPC batch rejection][beta-batches] | inbound | Covered | `internal/jsonrpc/conn_test.go`, `e2e/protocol_test.go`, `e2e/prompt_response_order_test.go` |

## `session/update` variants

| Variant | Status | Evidence |
| --- | --- | --- |
| `user_message` | Covered | `internal/agent/prompt_images_test.go`, `e2e/lifecycle_test.go` |
| `user_message_chunk` | Deferred #148 | single-message echo is used |
| `agent_message` | Covered | `e2e/replay_test.go` |
| `agent_message_chunk` | Covered | `e2e/lifecycle_test.go` |
| `agent_thought` | Covered | `e2e/replay_test.go` |
| `agent_thought_chunk` | Covered | `e2e/lifecycle_test.go` |
| `state_update` (running, requires_action, idle) | Covered | `e2e/lifecycle_test.go`, `e2e/permission_test.go` |
| `tool_call_update` | Covered | `e2e/tools_test.go` |
| `tool_call_content_chunk` | Deferred #147 | no streaming producer |
| `terminal_update` | Covered | `e2e/terminal_test.go`, `internal/acp/terminal_test.go` |
| `terminal_output_chunk` | Covered | `e2e/terminal_test.go`, `internal/agent/terminal_test.go` |
| `plan_update` | Covered | `internal/acp/plan_test.go`, `e2e/mode_test.go` |
| [`plan_removed`][beta-plan] | Covered for unfinished plans on explicit cancellation (ADR 0041) | `e2e/plan_test.go`, `e2e/plan_unix_test.go`, `internal/agent/plan_test.go` |
| `available_commands_update` | Deferred #146 | the command list is static (ADR 0015) |
| `config_option_update` | Covered for client setters | [Ordered complete snapshots](../../e2e/config_updates_test.go); agent-initiated catalog changes remain in #144 |
| `session_info_update` | Covered | `e2e/sessions_test.go` |
| `replay_complete` | Covered | `e2e/replay_test.go`, `internal/agent/resume_setup_test.go` |
| `usage_update` | Covered | `e2e/usage_test.go` |
| `notice` | Covered | `e2e/turn_timeout_test.go`, `e2e/store_test.go` |
| `compaction_update` | Covered | `e2e/compaction_test.go`, `e2e/commands_test.go` |
| `compaction_summary_chunk` | Deferred #149 | snapshot form only |

## Capabilities

| Capability | Status | Evidence |
| --- | --- | --- |
| Agent: session delete, additional directories (list, resume and close are baseline methods) | Covered | `e2e/protocol_test.go` asserts the advertised object |
| Agent: MCP stdio and HTTP | Covered | `e2e/mcp_test.go`, `e2e/mcp_http_test.go` |
| Agent: prompt image, embedded context | Covered | `e2e/protocol_test.go`, `e2e/prompt_images_test.go` |
| Agent: prompt audio | Deferred #153 | not advertised |
| Client: form elicitation | Covered | `e2e/elicitation_test.go` |
| Client: URL elicitation | Deferred #140 | not read or used |
| Auth capabilities | Excluded | ADR 0023 |

## Content

| Surface | Status | Evidence |
| --- | --- | --- |
| `text`, `resource_link`, embedded text resource | Covered | `internal/acp/content.go`, `e2e/lifecycle_test.go` |
| `image` PNG/JPEG (prompts, MCP results) | Covered | `e2e/prompt_images_test.go`, `e2e/mcp_images_test.go` |
| Other image formats | Deferred #151 | |
| `audio` | Deferred #152–#155 | MCP audio results are omitted, not mis-encoded |
| Embedded blob resources | Deferred #156 | |
| Assistant image/audio output | Deferred #155 | |
| Unknown fields and `_meta` on prompt blocks | Preserve on echo | `e2e/protocol_test.go`, `internal/agent/prompt_images_test.go` |
| Rejection of unadvertised prompt block types | Gap (beta.4 audit) | the agent currently echoes these blocks |

## Forms, permissions, configuration

| Surface | Status | Evidence |
| --- | --- | --- |
| String, integer, number, boolean, single and multi-select fields, titled choices, defaults, bounds | Covered | `internal/agent/form_constraints_test.go`, `internal/agent/typed_questions_test.go` |
| String `pattern` | Removed from beta.4 | acplause sends none. Revisit the premise of [#159](https://github.com/anesthetised/acplause/issues/159). |
| Session scope and request scope | Deferred #139 | |
| Permission subjects `tool_call`, `command` | Covered | `e2e/permission_test.go` |
| Permission kinds `allow_once`, `reject_once` | Covered | `e2e/permission_test.go` |
| Permission kinds `allow_always`, `reject_always` | Covered for `bash` | `internal/agent/remember_test.go`, `e2e/remembered_permission_test.go` |
| Select config options (model, effort, mode), groups | Covered | `e2e/catalog_test.go`, `e2e/selection_test.go` |
| Boolean config options | Deferred #143 | |
| `command` slash command advertisement | Covered | `e2e/commands_test.go` checks `/compact` on new and resume |
| `text` command input | Deferred #145 | |

## Beta.4 audit

For [#217](https://github.com/anesthetised/acplause/issues/217), the comparison
used the schema and protocol text from the alpha.9 and beta.4 pinned commits.
The stricter outgoing validation passes with the `command` discriminator on
both setup responses. The schema also tightens form scope and schema roots,
grouped select options, and diff changes. The existing tool-scoped forms,
select groups and diffs satisfy those shapes in e2e. Widened fields such as
signed terminal exit codes and compaction status patches still accept the
values acplause emits. System prompts, individual tool and terminal
cancellation, subagents and background work have no advertised producer or
capability here. [Upstream schema changelog][beta-changelog]

Both setup methods reject an unsupported non-null `systemPrompt` with `-32602`
before starting session setup or MCP connections. Tests cover cold and active
resumes, replay ordering, and omitted or null prompts.
[Stdio coverage](../../e2e/system_prompt_test.go) · [System prompt][beta-system-prompt]

Session setup advertises an effective model from the available catalog. New
sessions prefer the remembered choice and cold resumes prefer their saved
model. Both use `ACPLAUSE_DEFAULT_MODEL` when the saved choice is unavailable.
Setup fails if neither choice is usable. Tests cover first prompts without a
setter, restored selections, and failed restores that preserve history.
[ADR 0039](../adr/0039-model-default-policy.md) · [Stdio tests](../../e2e/selection_test.go) · [Alpha.9 defaults][alpha-defaults] · [Beta.4 defaults][beta-defaults]

Prompt retries share one insertion and retain the original message ID across
compaction and restart. Coverage includes validation, pending setup, cancellation
during commit, and retry after a lost response. [ADR 0033](../adr/0033-prompt-idempotency-keys.md) · [Retrying prompts][beta-retry]

Cursor resumes mark completed history before current-state synchronization and
live updates. The replay tests cover empty history, streaming through MCP
setup, client request ordering, and truncated delivery. Cold sessions accept
prompts after their successful resume response. An earlier prompt receives
`-32002` with the requested `data.sessionId`, including after the marker.
[Replay completion][beta-replay] · [Unknown sessions][beta-errors]

Foreground cancellation, deadline expiry and session close cancel pending
permission requests by ID. The stdio tests cover late replies, session
isolation and close ordering. The transport tests saturate both queues and
exercise response races and shutdown. [ADR 0034](../adr/0034-permission-request-cancellation.md) · [Cancellation][beta-cancel]

Initialization rejects other requests with `-32600` before dispatch and
allows repeats until a valid non-initialize request arrives. The agent reads
the version first and ignores unreadable fields during downgrade. Stdio tests
cover the repeat window, including unknown requests and invalid parameters.
[Stdio tests](../../e2e/protocol_test.go) · [Initialization guard][beta-init] · [Version negotiation][beta-version]

JSON-RPC dispatch rejects invalid request IDs and request or notification
forms before invoking a handler. It echoes valid IDs in their original spelling
and uses `id: null` when an error cannot identify a request. Invalid responses
leave pending calls available for valid replies. Explicit `params: null` remains
equivalent to omitted parameters for existing clients.
[Transport tests](../../internal/jsonrpc/envelope_test.go) · [Stdio tests](../../e2e/envelope_test.go) · [Communication model][beta-envelope] · [Beta.4 schema][beta-schema]

Prompt validation rejects unadvertised content types and malformed known fields,
including negative resource link sizes, before insertion. Accepted content keeps
its opaque fields in echo and replay. Stored opaque block types remain replayable.
Empty prompt arrays remain accepted as permitted by beta.4.
[Stdio coverage](../../e2e/prompt_content_test.go) · [Prompt contents][beta-prompt]

MCP setup rejects unsupported transports and duplicate names before connecting
peers. The streamable HTTP transport applies its own headers in preference to
configured values. Tests cover both setup methods and credential handling.
[ADR 0036](../adr/0036-validate-mcp-transport-at-setup.md) · [Stdio coverage](../../e2e/mcp_setup_validation_test.go) · [MCP servers][beta-mcp]

### Other follow-ups

- **Mixed-content command dispatch.** Alpha.9 already allowed commands to
  accompany other content. Beta.4
  specifies how to recognize a command in the first text block.
  Acplause recognizes `/compact` at the start of the first text block.
  New invocations with arguments or additional blocks receive `-32602` before
  insertion. Accepted commands retain the original content and message ID
  in replay.
  [ADR 0037](../adr/0037-compact-prompt-admission.md) · [Stdio tests](../../e2e/commands_test.go) · [Alpha.9 commands][alpha-commands] · [Beta.4 commands][beta-commands]
- **MCP partial setup.** The spec recommends continuing with the peers that
  did connect and notifying the user about failures. Both setup paths now
  publish the successful subset. A session can use built-in tools when all peers
  fail. One sanitized warning follows the setup response on the live stream.
  [ADR 0038](../adr/0038-partial-mcp-setup.md) · [Stdio tests](../../e2e/mcp_partial_setup_test.go) · [MCP servers][beta-mcp]
- **Configuration.** The spec recommends broadcasting the complete config when
  it changes, including client-initiated changes. [Set config](../../internal/agent/agent.go)
  now sends the complete list in a notification before the matching response
  when a setter changes the advertised configuration. Agent-initiated catalog
  refresh remains a [#144](https://github.com/anesthetised/acplause/issues/144) follow-up.
  [ADR 0040](../adr/0040-client-configuration-updates.md) · [Stdio tests](../../e2e/config_updates_test.go) · [Config changes][beta-config]
- **Session info.** Resume should send a current `session_info_update` even
  without replay, and active sessions should get updates when metadata
  changes. Both resume paths now report metadata immediately before current
  state, after the replay marker when requested. Live activity sends timestamp
  patches using the same values as `session/list` after a save.
  [Stdio tests](../../e2e/sessions_test.go) · [State synchronization][beta-sync] · [Session listing][beta-list]
- **Cancelled plans.** Agents should send `plan_removed` when a plan is
  cancelled or abandoned. acplause saves the removal of the current unfinished
  plan on explicit `session/cancel` before publishing it. Replay preserves the
  removal, including after an abrupt exit. A failed save preserves the plan.
  [ADR 0041](../adr/0041-remove-explicitly-cancelled-plans.md) · [Removing plans][beta-plan]


## Remaining blockers

#157 stays open: its dependencies #139–#140, #143–#156 are parked, so the
matrix cannot be final. Each deferral is a maintainer decision recorded on its
issue. Item 2 of the issue (optional-field, null and `_meta` audit) has not
been done beyond the pointers above; file a focused issue for any gap found.

[beta-schema]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/schema/v2/schema.json
[beta-changelog]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/schema/v2/CHANGELOG.md
[beta-prompt]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/prompt-lifecycle.mdx#1-user-message
[beta-retry]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/prompt-lifecycle.mdx#retrying-a-prompt
[beta-replay]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#replay-complete
[beta-batches]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/transports.mdx#no-batches
[beta-cancel]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/cancellation.mdx
[beta-errors]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/error.mdx#unknown-sessions
[beta-envelope]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/overview.mdx#communication-model
[beta-init]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/initialization.mdx#before-initialization-completes
[beta-version]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/initialization.mdx#version-negotiation
[alpha-commands]: https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/slash-commands.mdx#running-commands
[beta-commands]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/slash-commands.mdx#running-commands
[beta-system-prompt]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#system-prompt
[beta-mcp]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#mcp-servers
[beta-config]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-config-options.mdx#from-the-agent
[alpha-defaults]: https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/session-config-options.mdx#default-values-and-graceful-degradation
[beta-defaults]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-config-options.mdx#default-values-and-graceful-degradation
[beta-sync]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#current-state-synchronization
[beta-list]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-list.mdx#updating-session-metadata
[beta-plan]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/agent-plan.mdx#removing-plans
