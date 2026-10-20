<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/acplause-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/acplause-light.svg">
  <img alt="acplause" src="docs/assets/acplause-light.svg" width="340">
</picture>

*Don't hold your acplause — it streams.*

[![CI](https://github.com/anesthetised/acplause/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/anesthetised/acplause/actions/workflows/ci.yml)
[![Go coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/anesthetised/acplause/badges/coverage.json)](https://github.com/anesthetised/acplause/actions/workflows/ci.yml)

acplause is an [Agent Client Protocol](https://agentclientprotocol.com) v2
agent. Your editor talks to it over stdio; it thinks with any
OpenAI-compatible Chat Completions API, [OpenRouter](https://openrouter.ai)
first, and works in your workspace through tools.

- ACP v2 only, validated against the pinned schema (`testdata/schema/v2`).
- Streaming text and reasoning, tool calls, usage and cost reporting.
- Live bash terminal output with bounded snapshots for session replay.
- Asks permission before changing files or running commands.
- Prompt queue, cancellation that aborts the HTTP stream and kills
  everything a command started, session list/resume/close/delete.
- Sessions survive restarts and can move between editor windows.
- Model and reasoning effort selection from OpenRouter's model list.
- PNG/JPEG prompt attachments and MCP tool images for models known to accept images.
- Session-owned MCP tools over stdio and Streamable HTTP.
- Structured user questions on clients advertising ACP form elicitation.
- Native ACP execution plans with persistent progress snapshots.
- Per-session Plan/Build modes with enforced tool restrictions.
- A discoverable `/compact` command to summarize older model context on demand.

## Install

```sh
go install github.com/anesthetised/acplause/cmd/acplause@latest
```

## Configure

| Variable                | Default                         |                                       |
| ----------------------- | ------------------------------- | ------------------------------------- |
| `ACPLAUSE_API_KEY`      | `$OPENROUTER_API_KEY`           | API key                               |
| `ACPLAUSE_BASE_URL`     | `https://openrouter.ai/api/v1`  | any OpenAI-compatible API root        |
| `ACPLAUSE_MAX_ROUNDS`   | `200`                           | model requests per turn; `0` disables |
| `ACPLAUSE_BASH_TIMEOUT` | `30m`                           | bash tool timeout; `0` disables       |
| `ACPLAUSE_MCP_TIMEOUT`  | `1h`                            | MCP call timeout; `0` disables        |
| `ACPLAUSE_TURN_TIMEOUT` | `0`                             | accepted turn timeout; `0` disables   |
| `ACPLAUSE_DATA_DIR`    | `~/.acplause`                    | session database directory            |
| `ACPLAUSE_LOG_LEVEL`    | `info`                          | logs go to stderr                     |

Point your ACP v2 client at the `acplause` binary with these variables set.
The defaults allow sustained tool work, long builds and tests, and delegated
MCP tasks. An unset or empty variable selects its default; explicit `0`
disables only that limit. Negative values are rejected. Cancellation still
stops a turn and its active tools when limits are disabled. MCP setup,
discovery and cleanup keep their separate deadlines. Summary requests also
count toward `ACPLAUSE_MAX_ROUNDS` when it is enabled. See
[ADR 0008](docs/adr/0008-long-task-limits.md).

`ACPLAUSE_TURN_TIMEOUT` accepts a nonnegative Go duration such as `15m`.
It starts after prompt insertion, excluding queue, setup and insertion waiting,
and resets for the next accepted prompt. It covers model requests, built-in/MCP
tools, permission/form waiting and automatic/manual compaction. Expiry cancels
active work and ends once with `idle(cancelled)`, preceded by a live warning
notice explaining the turn deadline. User/session cancellation takes priority
and does not produce that timeout notice. Notices are not conversation replay.

[ADR 0020](docs/adr/0020-turn-budgets.md) specifies the opt-in turn-budget
policy. Usage availability and the time limit are implemented; token/USD
settings and enforcement remain inactive until #83. Current request/tool
defaults and MCP setup/cleanup deadlines remain unchanged.

The client receives the available models in `session/new` and selects one
with `session/set_config_option`. Catalogs with multiple model-ID namespaces
use labeled groups (for example `openai/…` and `anthropic/…`); unprefixed IDs
appear under "Other models". Single-group catalogs stay flat. Grouping changes
presentation only; model IDs and selection behavior stay the same. See
[ADR 0018](docs/adr/0018-group-model-options.md).

New sessions reuse the last selected model
and reasoning effort, remembered across restarts and editor windows in the
same data directory. If there is no remembered selection or the model is no
longer available, sessions show "Select a model"; prompts are rejected until
the client selects one. The agent never picks a substitute model. If the
provider's model list cannot be loaded or contains no models with tool
support, session creation fails. See [ADR 0005](docs/adr/0005-remember-model-selection.md).

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
worker's final save may briefly keep it busy after the idle update. Selecting
the current mode is idempotent. Plan is an agent tool policy, not an OS
sandbox: configured MCP servers can remain connected and setup/discovery
still runs. See [ADR 0013](docs/adr/0013-plan-build-mode.md).

## Tools

| Tool          | Kind      |                                                   |
| ------------- | --------- | ------------------------------------------------- |
| `read_file`   | `read`    | text files up to 1 MiB; `offset`, `limit` lines   |
| `list_dir`    | `read`    | tree, 3 levels, 200 entries                       |
| `search_code` | `search`  | literal or regex search, optional case folding, 50 matching lines |
| `get_instructions` | `read` | source-labelled root-to-directory `AGENTS.md` rules |
| `write_file`  | `edit`    | create or overwrite, up to 5 MiB                  |
| `edit`        | `edit`    | replace one exact occurrence                      |
| `edit_batch`  | `edit`    | 1–50 ordered replacements in one file, one approved diff |
| `bash`        | `execute` | `bash -c` in the working directory, output capped |
| `ask_user`    | `other`   | one to three text/choice questions, when supported |
| `update_plan` | `other`   | replace the current turn's execution plan          |

`search_code` is literal and case-sensitive by default: `{"pattern":"a.b"}`
matches the dot literally. Set `ignore_case: true` for Unicode case-insensitive
matching, or use `{"pattern":"^(TODO|FIXME):","regex":true,"ignore_case":true}`
to match a Go regular expression on each line. Results keep file order and line
numbers, with at most 50 matching lines and a 300-byte UTF-8-safe text preview
per line. `path` and `file_glob` narrow the search; text files are limited to
1 MiB. These options are also available in Plan mode.

To continue a search, set `offset` (zero-based matching-line offset) and `limit`
(1–50, default 50). `context_lines` (0–5, default 0) adds surrounding lines;
overlapping windows within a file are merged. Matches use `path:line:text`,
while context uses `path-line-text`. Only selected matching lines count toward
the page; a skipped or later match may appear as context and be selected on
another page. Every line keeps its original number and the same preview bound.
For example, `{"pattern":"TODO","limit":10,"context_lines":2}` returns at most
ten matches plus context. When any pagination/context option is explicit,
the result ends with `... more matches; next offset: N` or `... no more matches`;
an empty page reports `No more matches` with its offset. Calls omitting all three
options keep the original output format. Each page traverses files afresh in
lexical order, so edits between calls can shift offsets; pagination is not a
filesystem snapshot. Explicit null, fractional and out-of-range values fail
the tool without running the search.

`search_code` and `list_dir` skip hidden descendants by default. An explicit
hidden starting directory such as `{"path":".github"}` is inspectable;
`include_hidden: true` also reveals hidden descendants and can expose hidden
configuration to the model. Every `.git` entry is excluded case-insensitively,
including worktree metadata files. Explicitly traversing a `.git` path fails
the tool. Generated names (`node_modules`, `target`, `__pycache__`) remain
excluded during traversal, and directory symlinks are not followed. This policy does not interpret
`.gitignore` and does not restrict direct `read_file` or `bash` access.

`write_file`, `edit` and `edit_batch` report what they changed as a diff in the tool call,
with a `git_patch` unless the change is too large to diff.

`edit_batch` takes a required `path` and `edits` array of 1–50 objects with
explicit string `old_text` and `new_text`. It operates on one existing regular
UTF-8 file; input and final output must each fit within 5 MiB. Replacements
apply in array order to an in-memory candidate, and each nonempty `old_text`
must occur exactly once at its step, including overlapping occurrences. An
empty `new_text` deletes the match. For example:

```json
{"path":"main.go","edits":[{"old_text":"oldName","new_text":"newName"},{"old_text":"newName()","new_text":"newName(ctx)"}]}
```

The whole batch is validated before one diff is shown for one permission.
Preview retains the original bytes and the final candidate. Before writing,
the tool rereads the bounded file and compares its bytes with that original.
A changed, deleted or nonregular target fails without writing and requires a
new read, call and approval. Failed validation or a rejection/cancellation while
permission is pending never writes. The successful diff describes the retained candidate that was
approved. This is validation before writing, not crash-atomic persistence or a
lock against external changes racing after the final comparison.

`write_file`, `edit`, `edit_batch` and `bash` run only after the user allows them through
`session/request_permission`, with the diff or command shown in the request.
acplause offers "allow once" and "reject once"; anything else, including a
cancelled or unrecognised answer, is a rejection, which the model is told
about. Automatic approval is the client's setting. See
[ADR 0003](docs/adr/0003-permissions.md).

Permitted `bash` commands stream combined stdout/stderr into an agent-owned,
noninteractive ACP terminal. The client gets the command, working directory,
a terminal reference on the tool call, byte chunks and a final replacement
snapshot with the process's exit code or signal. Rejected commands and failed
process starts create no terminal. Commands continue to use the existing
permissions, timeout and process-group cancellation policy.

Terminal output retains the first 50,000 bytes; excess bytes are counted and
reported by an omission marker in the final snapshot. Live updates publish at
most every 100 ms in chunks up to 16 KiB. Resume replays one bounded snapshot
per terminal, ordered with ongoing output. Restarts preserve saved output and
mark interrupted terminals ended with unknown exit status; recent unsaved
chunks may be lost. The model still receives the usual bounded textual tool
result. Terminals provide no stdin, PTY or detached jobs. See
[ADR 0014](docs/adr/0014-stream-agent-terminals.md). Actual T3 streaming,
cancellation and restart replay were verified in the
[runtime smoke record](docs/testing/t3-first-class-smoke.md).

File tools are confined to the session's `cwd` and `additionalDirectories`
with `os.Root`, which blocks escapes through `..` and symlinks. `bash` is not
sandboxed.

Read, write and edit operations accept regular files only; pipes and devices
are rejected. `write_file.content` and `edit.new_text` must be explicit
strings: an empty string is valid, but an omitted or null value is an error.

`AGENTS.md` at the root of the session's `cwd` is appended to the system
prompt and reread before each normal model round and once before manual
`/compact`, on the session worker. One compaction keeps a stable instruction
snapshot across its summary requests. Missing or deleted files clear prior
root instructions; unreadable, nonregular, invalid UTF-8 or oversized files
(over 32 KiB) are omitted with a warning that does not include their contents.
Restarts reread live rules before the next model request.

Use `get_instructions` with a required directory `path`, such as
`{"path":"internal/agent"}`, to read scoped rules before editing there. It
returns source-labelled `AGENTS.md` files from the selected workspace root to
that directory, in root-to-directory order. Later sources take precedence only
in their subtree. Relative paths start at cwd; absolute paths select the first
configured containing root, with no combination of unrelated roots. Lookup
checks at most 32 directories, with 32 KiB per file and 128 KiB total text.
Missing files are normal; invalid targets, unreadable sources or exceeded bounds
fail the tool without a partial chain. Each lookup rereads live files; saved
tool results still replay their original content.

This read tool needs no permission in Plan or Build. Lookup is explicit: there
is no recursive discovery, parent lookup above a workspace root, `CLAUDE.md`
fallback or watcher. Project prose does not override enforced tool policies,
and explicit lookup cannot guarantee that arbitrary shell or MCP writes obey
scoped rules. See [ADR 0019](docs/adr/0019-scoped-project-instructions.md).

A session is titled after the first line of its first prompt. The title is
sent once, as a `session_info_update`, when that prompt is accepted, and is
returned by `session/list`. It is not replayed on resume: replay is
conversation history, and the title is metadata that `session/list` reports.

For an individual prompt request, the acceptance response precedes its
`user_message` and later turn updates. JSON-RPC batches receive one response
array after every request in the batch settles; updates may precede that array.

Prompts may embed resources (`session.prompt.embeddedContext`). A text
resource reaches the model as `<resource uri="...">` with its contents; a blob
resource is replaced by a one-line note naming its URI. The client still gets
every block echoed back unchanged.

In T3, select a model whose provider metadata explicitly lists image input,
then paste a screenshot into the composer or upload a PNG/JPEG attachment
and send the prompt. T3 supplies the image bytes over ACP; a file path in a
text prompt does not attach its pixels. Image-only prompts and multiple
images are accepted. A session without a textual title is named "Image prompt".
Actual T3 clipboard paste, JPEG upload, attachment-only composer input and
multiple images were verified with random visual markers in the
[runtime smoke record](docs/testing/t3-first-class-smoke.md). Automated tests
cover the ACP-to-provider path. See [ADR 0010](docs/adr/0010-image-prompts.md).

Images are limited to 4 MiB decoded bytes and 16 million pixels each, four
images and 8 MiB decoded total per prompt. Invalid images, no model selection,
and models with text-only or missing image metadata fail before the prompt
is accepted. The agent never switches models on your behalf. Switching an
existing image conversation to a non-vision model sends explicit omission
markers for historical images; switching back restores their pixels in model
requests. Audio and other image formats are not supported.

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
[ADR 0011](docs/adr/0011-structured-user-questions.md),
[ADR 0017](docs/adr/0017-multiselect-questions.md) and the
[T3 form smoke procedure](docs/testing/t3-first-class-smoke.md). Native T3 form
answers and turn cancellation were verified; the procedure distinguishes those
observations from extended lifecycle cases covered only by automated tests.

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
[ADR 0012](docs/adr/0012-structured-plans.md). Automated tests cover the protocol;
native T3 plan replacement, Plan/Build selection and plan replay after restart
were verified in the [runtime smoke record](docs/testing/t3-first-class-smoke.md).

Resuming a running session can replay partial messages and active tool calls.
Changes to additional directories apply to subsequent tool calls; a call
already running keeps its roots until it finishes.

## MCP servers

The ACP client supplies `mcpServers` in `session/new` and `session/resume`.
Both transports use the same discovery, permissions and tool history:

```json
{
  "mcpServers": [
    {
      "type": "stdio",
      "name": "workspace",
      "command": "/absolute/path/to/mcp-server",
      "args": ["--stdio"],
      "env": [{"name": "SERVER_TOKEN", "value": "client-supplied-token"}]
    },
    {
      "type": "http",
      "name": "remote",
      "url": "https://mcp.example.com/mcp",
      "headers": [{"name": "Authorization", "value": "Bearer client-supplied-token"}]
    }
  ]
}
```

Stdio executes the absolute command directly, without a shell, in the
session's `cwd`. It inherits only `HOME`, `PATH`, `USER`, `LOGNAME`,
`TMPDIR`/`TMP`/`TEMP`, `LANG` and `LC_*`, plus Windows system/profile
variables; explicit `env` entries override these. Provider API keys are not
inherited implicitly. MCP servers run with the agent's OS privileges,
outside the built-in file sandbox. Server stderr is discarded.

acplause requests MCP `2025-11-25` and also accepts `2025-06-18`. It reads
every tool-list page, retains input schemas, assigns stable aliases to avoid
name collisions, and refreshes changed catalogs. Every external call needs
explicit ACP permission, including tools marked read-only. Nested arguments,
structured-only output and opaque IDs remain JSON; the current model API
receives structured output as text, and ACP `rawOutput` retains its structure.

Catalog refresh revokes changed or removed definitions while a call awaits
permission or transport capacity. Dispatch is admitted atomically with refresh,
after an HTTP slot is acquired or the stdio writer selects the call, immediately
before I/O. An admitted call may finish on its original connection even if its
definition changes or disappears. Turn cancellation, timeout, connection
replacement, session close/delete and ownership loss still stop it. Admission
does not imply that the remote server received or completed the operation.

Resume replaces the entire MCP configuration; an omitted or empty list
removes all servers. Healthy unchanged connections are reused. Failed setup
preserves the previous configuration; later prompts wait for setup before
insertion. Changed/removed connections invalidate earlier model tool
bindings, so an old approval cannot invoke a replacement endpoint. Configs,
environment values, headers and upstream session IDs are kept in memory.
Tool results follow the normal conversation storage policy.

Setup allows 30 seconds, with at most four connections starting together;
discovery/refresh allows 15 seconds per server. `ACPLAUSE_MCP_TIMEOUT` starts
after permission and covers dispatch waiting and the response, independently
of the bash timeout. Cancellation stops local waiting promptly and sends a
best-effort MCP cancellation notification. A
remote side effect may still complete. Calls are never automatically repeated
after an ambiguous failure. Crashed stdio servers restart on explicit resume;
an expired HTTP session gets one recovery attempt for future model requests.

HTTP supports JSON and SSE responses, optional GET event streams (405 is
allowed), session/version headers and best-effort DELETE on close. Interrupted
SSE results resume only by GET with `Last-Event-ID`; a missing cursor fails
the call. Redirects are rejected. Legacy HTTP+SSE, OAuth, interactive auth,
roots, sampling, elicitation and MCP Tasks are not supported.

Close/delete/shutdown/ownership loss release connections. Stdio cleanup closes
stdin, waits one second, sends TERM, waits one second, then kills and reaps;
Unix cleanup includes the process group. Other platforms clean up the direct
child only. HTTP DELETE has a two-second deadline. Limits are 16 servers,
100 pages/1,024 tools per server, 8 MiB per message/event/catalog and 256 KiB
per projected text/JSON result; exceeding a limit fails explicitly.

Text, embedded text resources, resource links and validated PNG/JPEG tool
images are supported. Images reach the ACP client and the vision model, with
their originating tool-call IDs. Chat Completions receives all sibling tool
results before a separate user message labeled as untrusted tool-image context.
The model selected for that round must explicitly advertise image input;
text-only or unknown models get omission markers, without a model switch.

Tool images have the same per-image and per-result limits as prompt images:
4 MiB and 16 million pixels per image, four images and 8 MiB decoded total per
result. The 8 MiB MCP frame limit still applies before image validation.
Invalid, unsupported and excess images produce omission markers, retaining
usable text/JSON and images that fit. Audio and blob results remain omitted.
An unsupported-only result fails with a possible-completion warning; image
projection never repeats the remote operation. Images persist for resume and
replay, while summaries and ACP rawOutput carry descriptors without base64.
Actual T3 browser screenshots reached the vision model and fresh visual markers
were recognized. In the tested T3 build, the expanded ACP tool card displays
only the image descriptor, without an inline image; this client display gap is
tracked in [#71](https://github.com/anesthetised/acplause/issues/71), separately
from successful model delivery. See the
[runtime smoke record](docs/testing/t3-first-class-smoke.md).
See [ADR 0009](docs/adr/0009-image-tool-results.md),
[ADR 0007](docs/adr/0007-mcp-servers.md) and the
[real T3 smoke record](docs/testing/mcp-t3-smoke.md).

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
[ADR 0006](docs/adr/0006-context-compaction.md) for limits and recovery behavior.

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
[ADR 0015](docs/adr/0015-compact-slash-command.md). In the tested T3 Code (Beta)
0.1.0 build, the advertised command is absent from the slash menu and submission
fails before ACP dispatch; see [#70](https://github.com/anesthetised/acplause/issues/70).
The [runtime smoke record](docs/testing/t3-first-class-smoke.md) distinguishes
shipped ACP behavior from client compatibility. Full T3 acceptance
[#38](https://github.com/anesthetised/acplause/issues/38) remains open for this
and tool-image display. Checkpoint rollback, native subagent lineage, rewind,
active steering and auth are not provided by this ACP v2 roadmap.

## Storage

Sessions are stored in `~/.acplause/acplause.db`; `ACPLAUSE_DATA_DIR` selects
another directory. acplause creates the directory with permissions `0700`
and the database with `0600`. The database holds prompts, model output and
tool output in plain text, along with workspace paths, titles, timestamps,
model and effort selections, and accumulated cost. Prompt and tool images are
stored inline in model history and the original ACP transcript until session deletion;
these copies increase database size, and there is no session-wide storage
quota. See [ADR 0004](docs/adr/0004-sqlite-storage.md).

`session/list` includes stored sessions from every process using that data
directory. `session/resume` restores their conversation history and settings.
`session/close` cancels work and releases the session in this process; its
history stays listed and can be resumed. `session/delete` cancels local work
and removes the session and its stored history. Deleting an unknown or
already deleted session succeeds. Nothing expires automatically.

Two editor windows can work on different sessions at the same time. Resuming
or deleting a session owned by another window is allowed while it is idle,
and refused while it is working or waiting for permission or form answers.
After a takeover or deletion, the old window loses access at its next write or lease renewal
(every 10 seconds). A crashed process's lease expires within 30 seconds;
a resumed session then starts idle, with unfinished tool calls marked failed
and matching results added to the model history.

Changes are saved at prompt, model response, tool call and configuration
boundaries. Streaming chunks stay in memory until a save boundary, so a crash
can lose the response being streamed. Failure to open or migrate the database
prevents startup. A later save failure is logged to stderr and sends a live ACP
warning notice; work continues from memory, and unsaved changes may be missing
after a restart. Repeated failures send no additional notice until a save succeeds.
Notices are not stored or replayed, and clients may ignore them.

## Develop

```sh
just check   # gofmt, vet, staticcheck, tests with -race, go mod tidy
just e2e -v  # end-to-end tests only
```

GitHub Actions runs `just check` on every pull request and push to `main`.
CI reads the Go version from `go.mod` and pins just 1.59.0 and Staticcheck
2026.2.2 (v0.8.2).

CI collects Go statement coverage in the same test run, fails below 80%, and
publishes a shields.io endpoint file to the `badges` branch after successful
pushes to `main`, using only `GITHUB_TOKEN`. Coverage measures code run in the
test processes; the binary built separately by the e2e harness is not included.

The end-to-end tests in `e2e/` build the binary, drive it over stdio against
a scripted SSE server and validate every message it writes against the ACP
schema. Decisions are recorded in [`docs/adr`](docs/adr).

Notes for coding agents are in [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE)
