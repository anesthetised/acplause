# Using acplause

[Documentation](README.md) · [Project overview](../README.md)

## Model and reasoning selection

The client receives the available models in `session/new` and selects one
with `session/set_config_option`. Catalogs with multiple model-ID namespaces
use labeled groups (for example `openai/…` and `anthropic/…`); unprefixed IDs
appear under "Other models". Single-group catalogs stay flat. Grouping changes
presentation only; model IDs and selection behavior stay the same. See
[ADR 0018](adr/0018-group-model-options.md).

New sessions reuse the last selected model
and reasoning effort, remembered across restarts and editor windows in the
same data directory. If there is no remembered selection or the model is no
longer available, sessions show "Select a model"; prompts are rejected until
the client selects one. The agent never picks a substitute model. If the
provider's model list cannot be loaded or contains no models with tool
support, session creation fails; the next request retries the list, and a
loaded list is kept for the process lifetime. See [ADR 0005](adr/0005-remember-model-selection.md).

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
`ask_user` tool. It sends `elicitation/create` with one to three required fields:
text, single-choice strings, or multiple choices when a question sets
`multiSelect: true` with nonempty options. Multiple-choice answers are arrays of
distinct offered strings; an empty array explicitly selects none. Missing, null,
duplicate and unoffered selections fail validation. Only a complete accepted answer
with the requested keys and valid values reaches the model as answers; decline,
cancel, malformed responses and request failures become failed tool results.
While waiting, the tool stays pending and the session reports `requires_action`.
The tool instructs the model never to request secrets or credentials, including
passwords and API keys. When access needs configuring, the user should do so
outside the conversation and report only whether setup is complete.
Answers request information; file writes and commands still require permission.

Cancelling, closing or deleting the session stops waiting and sends best-effort
`$/cancel_request`. Late replies cannot restart work. Same-connection resume
replays the pending tool without reissuing the form; a process restart restores
finished results or fails interrupted questions. Form support is renegotiated on
every connection. Clients without it receive no `ask_user` tool and the model can
ask in text. Malformed optional capability values are treated as unsupported,
so they do not prevent initialization. This does not enable MCP server
elicitation. See
[ADR 0011](adr/0011-structured-user-questions.md),
[ADR 0017](adr/0017-multiselect-questions.md) and the
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
Resume replays the latest snapshot of each plan at its original conversation
position. Cancellation, errors and restarts preserve unfinished statuses.
Plans report progress without executing commands, changing files or asking
permission, and require no optional client capability. See
[ADR 0012](adr/0012-structured-plans.md). Automated tests cover the protocol;
native T3 plan replacement, Plan/Build selection and plan replay after restart
were verified in the [runtime smoke record](testing/t3-first-class-smoke.md).

## Context compaction

Long conversations automatically reduce model context when its estimated
input size exceeds 75% of the model's advertised window. Large tool results
are shortened oldest first; if that is insufficient, the same model summarizes
older history. The system instructions, latest user prompt and latest tool
exchange in the active turn are retained. The editor receives compaction
status and the summary, and replay keeps the original full conversation.
Summary requests count toward the request limit and usage cost. The portable
byte-based estimate is conservative; providers without a published context
length do not enable automatic compaction. See
[ADR 0006](adr/0006-context-compaction.md) for limits and recovery behavior.

### Manual compaction

Send `/compact` as a single text-only prompt to summarize older history now,
even below the automatic threshold. It runs in the prompt queue with the
selected model, reports compaction progress and a short confirmation, and
keeps the original full conversation for replay. Summary requests count
toward usage, cost and the turn's request limit; the command does not run an
ordinary answer or tools afterwards. With no older history or no published
context window, it returns a short explanation without contacting the model.
Cancellation and summary failures keep the pre-summary history (apart from
any completed tool-output trimming). `/compact explanation`, `/compactness`,
resource contents and prompts mixing text with attachments remain ordinary
prompts. The command is advertised on session creation and resume; see
[ADR 0015](adr/0015-compact-slash-command.md). In the tested T3 Code (Beta)
0.1.0 build, the advertised command is absent from the slash menu and submission
fails before ACP dispatch; see [#70](https://github.com/anesthetised/acplause/issues/70).
The [runtime smoke record](testing/t3-first-class-smoke.md) distinguishes
shipped ACP behavior from client compatibility. Full T3 acceptance
[#38](https://github.com/anesthetised/acplause/issues/38) remains open for this
and tool-image display. Checkpoint rollback, native subagent lineage, rewind,
active steering and auth are not provided by this ACP v2 roadmap.
