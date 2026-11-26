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
| `available_commands_update` | Deferred #146 | the command list is static (ADR 0015) |
| `config_option_update` | Gap (beta.4 audit) | client-initiated changes return config in the response |
| `session_info_update` | Covered | `e2e/sessions_test.go` |
| `replay_complete` | Gap [#219](https://github.com/anesthetised/acplause/issues/219) | `internal/agent/session.go` sends replay without a marker |
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

Prompt retries share one insertion and retain the original message ID across
compaction and restart. Coverage includes validation, pending setup, cancellation
during commit, and retry after a lost response. [ADR 0033](../adr/0033-prompt-idempotency-keys.md) · [Retrying prompts][beta-retry]

Foreground cancellation, deadline expiry and session close cancel pending
permission requests by ID. The stdio tests cover late replies, session
isolation and close ordering. The transport tests saturate both queues and
exercise response races and shutdown. [ADR 0034](../adr/0034-permission-request-cancellation.md) · [Cancellation][beta-cancel]

### Tracked follow-ups

- **Replay boundary.** Cursor-based resumes must mark completed history
  exactly once, before current-state synchronization and live updates.
  [Resume](../../internal/agent/session.go) appends current state to the
  transcript snapshot and starts deferring only when the snapshot is ready.
  [#219](https://github.com/anesthetised/acplause/issues/219) covers the marker
  and the live-update cut point, including empty replay. Its tests should also
  exercise slow resume setup while live updates and client requests are pending.
  [Replay completion][beta-replay]
- **Inactive and missing sessions.** [#222](https://github.com/anesthetised/acplause/issues/222)
  adds the requested `data.sessionId` to applicable `-32002` responses and
  makes closing a missing or inactive session succeed. The agent still queues
  prompts behind an in-flight resume for an inactive session. Beta.4 requires
  those prompts to receive `-32002` until the resume succeeds. Verify this
  activation boundary in [#219](https://github.com/anesthetised/acplause/issues/219).
  [Unknown sessions][beta-errors] · [Replay completion][beta-replay]

### Other follow-ups

- **Request envelopes.** Request IDs must be strings or integers, with
  `null` reserved for errors where the request ID cannot be determined.
  [ID decoding](../../internal/jsonrpc/message.go) accepts fractions and nulls,
  and [dispatch](../../internal/jsonrpc/conn.go) uses the same method table for
  requests and notifications. Validate request IDs and known method envelopes
  at ingress. Test that error responses can still use `id: null`.
  [Communication model][beta-envelope] · [Beta.4 schema][beta-schema]
- **Initialization.** Before initialization any other request must get
  `-32600`. A repeat after another request must also get `-32600`. Version
  negotiation must read the version first and ignore unreadable fields when
  negotiating down. [Dispatch](../../internal/jsonrpc/conn.go) answers unknown
  methods before the [agent's initialize guard](../../internal/agent/agent.go),
  and [parameter validation](../../internal/acp/initialize.go) requires `info`
  before version selection. Add stdio cases for each ordering rule and for
  malformed fields in a newer client's request.
  [Initialization guard][beta-init] · [Version negotiation][beta-version]
- **Prompt contents and command dispatch.** The Agent must reject unadvertised
  or unknown content before insertion. [Prompt decoding](../../internal/acp/content.go)
  preserves it, and [model projection](../../internal/agent/text.go) skips it
  while the echo still includes it. Add rejection tests for audio and custom
  blocks alongside preservation tests for unknown fields on supported blocks.
  The schema now requires a nonempty prompt and the protocol permits agents
  to reject the empty arrays the decoder currently accepts. Check validation
  of known block fields too, including the new nonnegative `resource_link.size`.
  Alpha.9 already allowed commands to accompany other content. Beta.4
  specifies how to recognize a command in the first text block.
  [The parser](../../internal/agent/commands.go) still requires a single
  text block. Define and test how argument-free `/compact` should handle
  accompanying content.
  [Prompt contents][beta-prompt] · [Alpha.9 commands][alpha-commands] · [Beta.4 commands][beta-commands]
- **Unsupported system prompts.** Beta.4 requires `-32602` when the agent
  cannot apply a supplied `systemPrompt` because its value uses an unsupported
  mode or content type. [Both setup request types](../../internal/acp/session.go)
  silently drop the field. Add focused tests that an unsupported prompt
  leaves the session uncreated or unresumed in `session/new` and
  `session/resume`. [System prompt][beta-system-prompt]
- **MCP setup.** Agents must reject unsupported transports and duplicate names,
  and transport-owned HTTP headers use transport values. [MCP validation](../../internal/acp/mcp.go)
  skips unrecognized transports and rejects transport-owned headers. Add setup
  tests for unsupported transports and reserved headers. The spec also
  recommends continuing with the peers that did connect and notifying the user
  about failures. [New](../../internal/agent/agent.go) and
  [resume](../../internal/agent/session.go) currently fail setup if a peer cannot
  connect. Decide how to handle partial success and test it with one working
  and one failing peer. [MCP servers][beta-mcp]
- **Configuration.** The spec recommends broadcasting the complete config when
  it changes, including client-initiated changes. [Set config](../../internal/agent/agent.go)
  only returns the response. Expand the scope of [#144](https://github.com/anesthetised/acplause/issues/144)
  to cover this and check ordering. Alpha.9 already required usable
  defaults. Beta.4 expresses that requirement through `currentValue`.
  [The model selector](../../internal/agent/catalog.go) offers
  an empty placeholder for a fresh session, and [prompt admission](../../internal/agent/session.go)
  requires a model selection. Resolve the default contract with ADR 0002
  before changing model selection. [Config changes][beta-config] · [Alpha.9 defaults][alpha-defaults] · [Beta.4 defaults][beta-defaults]
- **Session info.** Resume should send a current `session_info_update` even
  without replay, and active sessions should get updates when metadata
  changes. [The session](../../internal/agent/session.go) sends state on
  resume and only reports info when assigning the first title. Test that a
  reconnect receives the current title and update time before the response.
  [State synchronization][beta-sync] · [Session listing][beta-list]
- **Cancelled plans.** Agents should send `plan_removed` when a plan is
  cancelled or abandoned. [Item plans](../../internal/agent/plan.go) are stored
  as replay snapshots, with no removal in [turn finalization](../../internal/agent/session.go).
  Define when a plan is abandoned and test that replay preserves removals.
  [Removing plans][beta-plan]

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
