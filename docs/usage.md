# Using acplause

[Documentation](README.md) · [Project overview](../README.md)

## Model and reasoning selection

The client receives the effective model and the available choices in
`session/new` and can change the model with `session/set_config_option`.
Catalogs with multiple model-ID namespaces use labeled groups (for example
`openai/…` and `anthropic/…`). Unprefixed IDs appear under "Other models".
Single-group catalogs stay flat. See
[ADR 0018](adr/0018-group-model-options.md).

New sessions reuse the last explicitly selected model and valid reasoning
effort across restarts and editor windows in the same data directory. A cold
resume prefers that session's own saved model and effort. If the relevant
model is missing or unavailable, setup uses the configured
[`ACPLAUSE_DEFAULT_MODEL`](configuration.md#default-model) with the provider's
default reasoning effort. If neither choice is usable, setup fails with
instructions to configure a default. A restored session saves its replacement
model in that session. Only explicit client changes update the remembered
choice. See [ADR 0039](adr/0039-model-default-policy.md).

If the provider's catalog cannot be loaded or contains no models with tool
support, setup fails and the next request retries the list. A loaded list is
served for one hour. The next catalog lookup after that triggers a background
reload and uses the previous list for the current response. New sessions and
cold resumes after a successful reload see the new list. Open sessions keep
their list and selected model. A failed reload keeps the previous list and
logs a warning.

When a client changes a setting, it receives a `config_option_update` followed
by the setter response. Both carry the same complete list, including reasoning
effort options for the selected model. Selecting the current value returns the
complete list with no new notification. See [ADR 0040](adr/0040-client-configuration-updates.md).

## Plan and Build modes

The `mode` session config option selects **Build** (the default) or **Plan**.
Plan permits only `read_file`, `list_dir`, `search_code`, `get_instructions`,
`update_plan` and, on capable clients, `ask_user`. It rejects file changes, all shell commands
(including inspection commands) and all external MCP tool calls, even those
marked read-only, before requesting permission or executing them. Build
restores the usual tools and client-owned approval policy. Mode survives
resume and restarts for that session; new sessions always start in Build,
independently of remembered model and reasoning preferences.

A mode change requires an idle session with no queued prompts or pending
setup. While work, permissions or resume/setup are pending, a different mode
is rejected with `-32602`: cancel or wait for completion, then retry. The
idle update ends the turn: the mode can change while its final save finishes.
Selecting the current mode is idempotent. Plan is an agent tool policy, not an OS
sandbox: configured MCP servers can remain connected and setup/discovery
still runs. See [ADR 0013](adr/0013-plan-build-mode.md).

## Embedded resources

Prompts may embed resources (`session.prompt.embeddedContext`). A text
resource reaches the model as `<resource uri="...">` with its contents; a blob
resource is replaced by a one-line note naming its URI. The client still gets
every block echoed back unchanged.

## Prompt images

In T3, select a model whose provider metadata explicitly lists image input,
then paste a screenshot into the composer or upload a PNG/JPEG attachment
and send the prompt. T3 supplies the image bytes over ACP; a file path in a
text prompt does not attach its pixels. Image-only prompts and multiple
images are accepted. A session without a textual title is named "Image prompt".
Actual T3 clipboard paste, JPEG upload, attachment-only composer input and
multiple images were verified with random visual markers in the
[runtime smoke record](testing/t3-first-class-smoke.md). Automated tests
cover the ACP-to-provider path. See [ADR 0010](adr/0010-image-prompts.md).

Images are limited to 4 MiB decoded bytes and 16 million pixels each, four
images and 8 MiB decoded total per prompt. Invalid images, no model selection,
and models with text-only or missing image metadata fail before the prompt
is accepted. The agent never switches models on your behalf. Switching an
existing image conversation to a non-vision model sends explicit omission
markers for historical images; switching back restores their pixels in model
requests. Audio and other image formats are not supported.

## Structured questions

Clients advertising `capabilities.elicitation.form: {}` enable the agent-owned
`ask_user` tool. It sends `elicitation/create` with one to three fields, required
unless a question sets `required: false`. An optional question may be left
unanswered; unanswered fields are absent from the result (an accept without any
content means nothing was answered), and a default is never turned into an
answer. Each question may set `type` to `string` (the default), `integer`, `number` or
`boolean`. Integers require an integer JSON token in the signed 64-bit range;
decimal/exponent tokens and quoted numbers are not converted into integers.
Numbers accept finite floating-point values, including fractions. Numeric
tokens reach the model unchanged, including large integers. Boolean `false`
and numeric zero are valid answers; missing and null values are not.
Numeric and boolean fields have automated protocol coverage; their T3
rendering has not been verified.

String questions support text, single-choice strings, or multiple choices when
`multiSelect: true` is set with nonempty `options` or `choices`. Other field types cannot use
choices or multiselect. Multiple-choice answers are arrays of
distinct offered strings; an empty array explicitly selects none. Missing, null,
duplicate and unoffered selections fail validation. Only an accepted answer that
includes every required field, with only requested keys and valid values,
reaches the model as answers; decline,
cancel, malformed responses and request failures become failed tool results.
While waiting, the tool stays pending and the session reports `requires_action`.
The tool instructs the model never to request secrets or credentials, including
passwords and API keys. When access needs configuring, the user should do so
outside the conversation and report only whether setup is complete.
Answers request information; file writes and commands still require permission.

Questions can also set constraints, which the agent checks again on the answer:

- `title` and `description` on the form, and `default` on a question. Defaults
  are only a pre-fill for the client; they must satisfy the question's own
  constraints, and `0`, `false`, `""` and `[]` are valid defaults.
- Strings: `minLength`/`maxLength` (counted in Unicode code points) and `format`.
  `date` (`YYYY-MM-DD`), `date-time` (RFC 3339), `email` (a bare address) and
  `uri` (with a scheme) are validated; other formats are annotations. `pattern` is
  not supported and fails the call ([#159](https://github.com/anesthetised/acplause/issues/159)).
- Integers and numbers: inclusive `minimum`/`maximum`.
- Choices: `choices` is a list of `{value, title, description}` used instead of
  `options`, for single choice or `multiSelect`. Answers use the `value`, never the
  title. Multiselect also accepts `minItems`/`maxItems`.

Constraints that do not fit the question's type, inconsistent bounds, duplicate
choices and invalid defaults fail the call before any form is sent.

Cancelling, closing or deleting the session stops waiting and sends best-effort
`$/cancel_request`. Late replies cannot restart work. Same-connection resume
replays the pending tool without reissuing the form; a process restart restores
finished results or fails interrupted questions. Form support is renegotiated on
every connection. Clients without it receive no `ask_user` tool and the model can
ask in text. Malformed optional capability values are treated as unsupported,
so they do not prevent initialization. This does not enable MCP server
elicitation. See
[ADR 0011](adr/0011-structured-user-questions.md),
[ADR 0017](adr/0017-multiselect-questions.md),
[ADR 0027](adr/0027-typed-user-questions.md) and the
[T3 form smoke procedure](testing/t3-first-class-smoke.md). Native T3 form
answers and turn cancellation were verified; the procedure distinguishes those
observations from extended lifecycle cases covered only by automated tests.

## Execution plans

For multi-step work, the model can publish a native ACP plan with `update_plan`.
Each call replaces the complete list of one to twenty entries, with priorities
`high`, `medium` or `low` and statuses `pending`, `in_progress` or `completed`.
At most one entry may be in progress; each entry has nonblank content of up to
1000 characters. Invalid arguments fail the tool without changing the plan.

The agent assigns one plan ID per accepted user turn and returns it in the
tool result. Later updates in that turn reuse it; later turns create new plans.
Explicitly cancelling a turn with `session/cancel` removes that turn's plan
if any entry is pending or in progress and the removal saves successfully.
The agent saves the removal before reporting it to the client. If that save
fails, the turn still ends as cancelled and the plan stays visible. Completed
plans and plans from earlier turns stay visible. Close, disconnect, deadlines,
provider errors and restarts preserve the last saved plan on their own.
History replay sends the latest snapshot or removal at the plan's original
conversation position.
[ADR 0041](adr/0041-remove-explicitly-cancelled-plans.md)

Plans report progress without executing commands, changing files or asking
permission, and require no optional client capability. The T3 runtime smoke
record covers native plan replacement, Plan/Build selection and plan replay
after restart. [ADR 0012](adr/0012-structured-plans.md) ·
[T3 runtime smoke record](testing/t3-first-class-smoke.md)

## Context compaction

Long conversations automatically reduce model context when its estimated
input size exceeds 75% of the model's advertised window. Large tool results
are shortened oldest first; if that is insufficient, the same model summarizes
older history. The system instructions, latest user prompt and latest tool
exchange in the active turn are retained. The editor receives compaction
status and the summary, and replay keeps the original full conversation.
Summary requests count toward the request limit and usage cost. Until a
model has answered once, the estimate counts one serialized byte as one
token, which compacts early. After each response that reports
`prompt_tokens`, the agent learns that model's bytes per token, shared by all
sessions of the process and forgotten on restart, keeps the most
conservative ratio seen, and adds a 20% margin; the estimate never exceeds
one token per byte, and implausible ratios above 6 bytes per token are
ignored. Requests carrying images do
not calibrate; images keep a fixed reserve. Providers without a published
context length do not enable automatic compaction.

Each request asks for at most a quarter of the window as output
(`max_tokens`), capped by the provider's published output limit. Without a
published limit `max_tokens` is omitted and the provider default applies.
OpenRouter checks the balance against the requested output; with little
credit left it can answer 402 ("requires more credits, or fewer
max_tokens"). See [ADR 0006](adr/0006-context-compaction.md) and
[ADR 0031](adr/0031-calibrated-context-budget.md) for limits and recovery
behavior.

### Manual compaction

Send `/compact` by itself to summarize older history now,
even below the automatic threshold. It runs in the prompt queue with the
selected model, reports compaction progress and a short confirmation, and
keeps the original full conversation for replay. Summary requests count
toward usage, cost and the turn's request limit; the command does not run an
ordinary answer or tools afterwards. With no older history or no published
context window, it returns a short explanation without contacting the model.
Cancellation and summary failures keep the pre-summary history (apart from
any completed tool-output trimming).

The first text block must start with `/compact` followed by whitespace or the
end of the block. Trailing whitespace is allowed. Arguments such as
`/compact explanation` or any additional block, including an attachment before
the first text block, cause the agent to reject the prompt. Send the command
by itself, then send text or attachments separately. `/compactness`, a command
preceded by whitespace, slash text in a later text block, and unadvertised
commands remain ordinary prompts. The command is advertised on session
creation and resume. See [ADR 0015](adr/0015-compact-slash-command.md) and
[ADR 0037](adr/0037-compact-prompt-admission.md).

In the tested T3 Code (Beta) 0.1.0 build, the advertised command is absent
from the slash menu and submission fails before ACP dispatch. See
[#70](https://github.com/anesthetised/acplause/issues/70).
The [runtime smoke record](testing/t3-first-class-smoke.md) distinguishes
shipped ACP behavior from client compatibility. Full T3 acceptance
[#38](https://github.com/anesthetised/acplause/issues/38) remains open for this
and tool-image display. Checkpoint rollback, native subagent lineage, rewind,
active steering and auth are not provided by this ACP v2 roadmap.
