# ACP v2 conformance matrix (non-auth)

Tracks [#157](https://github.com/anesthetised/acplause/issues/157). Wire source:
`testdata/schema/v2/schema.json`, ACP `2.0.0-alpha.9` at `3be9cfc` (see
`testdata/schema/v2/VERSION`). This records protocol conformance only; actual
client rendering is tracked separately in
[t3-first-class-smoke.md](t3-first-class-smoke.md). It does not claim full
compliance: every surface is either covered or points at an open issue.

Method: the e2e harness (`e2e/`) validates every message the agent writes
against the schema, so a surface "covered by e2e" is also schema-validated.
Test names are file pointers, not an exhaustive list. Compiled 2026-10-22 from
the schema's `x-method` values, `SessionUpdate` variants and the Go types in
`internal/acp`.

Legend: **Covered** = implemented and tested; **Deferred #N** = not
implemented, parked by the maintainer (needs a design decision first);
**Excluded** = out of scope by ADR.

## Methods

| Method | Direction | Status | Evidence |
| --- | --- | --- | --- |
| `initialize` | client→agent | Covered | `e2e/protocol_test.go`, `internal/acp/acp_test.go` |
| `session/new` | client→agent | Covered | `e2e/sessions_test.go`, `internal/agent/methods_test.go` |
| `session/prompt` | client→agent | Covered | `e2e/lifecycle_test.go`, `e2e/prompt_images_test.go` |
| `session/cancel` | client→agent | Covered | `e2e/lifecycle_test.go`, `internal/agent/queue_test.go` |
| `session/list` | client→agent | Covered | `e2e/store_test.go` |
| `session/resume` | client→agent | Covered | `e2e/replay_unix_test.go`, `internal/agent/resume_setup_test.go` |
| `session/close` | client→agent | Covered | `e2e/sessions_test.go`, `e2e/issue50_test.go` |
| `session/delete` | client→agent | Covered | `e2e/delete_test.go` |
| `session/set_config_option` | client→agent | Covered (select options only) | `e2e/selection_test.go`, `e2e/mode_test.go`; boolean options Deferred #143 |
| `session/update` | agent→client | Covered (see variants) | `e2e/` replay and lifecycle tests |
| `session/request_permission` | agent→client | Covered | `e2e/permission_test.go`, `e2e/remembered_permission_test.go`; all four option kinds for `bash` (ADR 0029), once-only elsewhere |
| `elicitation/create` | agent→client | Covered for tool-call scope | `e2e/elicitation_test.go`, `e2e/form_constraints_test.go`; session/request scope Deferred #139 |
| `elicitation/complete` | agent→client | Deferred #140 | no non-auth producer |
| `$/cancel_request` | both | Covered | `internal/jsonrpc/cancellable_test.go`, `internal/jsonrpc/conn_test.go` |
| `auth/login`, `auth/logout` | client→agent | Excluded | ADR 0023 |
| JSON-RPC batches | both | Covered | `internal/jsonrpc/conn_test.go`, `internal/jsonrpc/issue54_test.go` |

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
| `config_option_update` | Deferred #144 | no agent-initiated change |
| `session_info_update` | Covered | `e2e/sessions_test.go` |
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
| Unknown content/`_`-variants and `_meta` | Covered | `e2e/protocol_test.go`, `internal/agent/prompt_images_test.go` |

## Forms, permissions, configuration

| Surface | Status | Evidence |
| --- | --- | --- |
| String, integer, number, boolean, single and multi-select fields, titled choices, defaults, bounds | Covered | `internal/agent/form_constraints_test.go`, `internal/agent/typed_questions_test.go` |
| String `pattern` | Deferred #159 | dialect undefined (ADR 0028) |
| Session scope and request scope | Deferred #139 | |
| Permission subjects `tool_call`, `command` | Covered | `e2e/permission_test.go` |
| Permission kinds `allow_once`, `reject_once` | Covered | `e2e/permission_test.go` |
| Permission kinds `allow_always`, `reject_always` | Covered for `bash` | `internal/agent/remember_test.go`, `e2e/remembered_permission_test.go` |
| Select config options (model, effort, mode), groups | Covered | `e2e/catalog_test.go`, `e2e/selection_test.go` |
| Boolean config options | Deferred #143 | |
| `text` command input | Deferred #145 | |

## Remaining blockers

#157 stays open: its dependencies #139–#140, #143–#156 are parked, so the
matrix cannot be final. Each deferral is a maintainer decision recorded on its
issue. Item 2 of the issue (optional-field, null and `_meta` audit) has not
been done beyond the pointers above; file a focused issue for any gap found.
