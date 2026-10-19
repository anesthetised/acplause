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

The client receives the available models in `session/new` and selects one
with `session/set_config_option`. New sessions reuse the last selected model
and reasoning effort, remembered across restarts and editor windows in the
same data directory. If there is no remembered selection or the model is no
longer available, sessions show "Select a model"; prompts are rejected until
the client selects one. The agent never picks a substitute model. If the
provider's model list cannot be loaded or contains no models with tool
support, session creation fails. See [ADR 0005](docs/adr/0005-remember-model-selection.md).

The `mode` session config option selects **Build** (the default) or **Plan**.
Plan permits only `read_file`, `list_dir`, `search_code`, `update_plan` and,
on capable clients, `ask_user`. It rejects file changes, all shell commands
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
| `search_code` | `search`  | plain-text search, 50 matches                     |
| `write_file`  | `edit`    | create or overwrite, up to 5 MiB                  |
| `edit`        | `edit`    | replace one exact occurrence                      |
| `bash`        | `execute` | `bash -c` in the working directory, output capped |
| `ask_user`    | `other`   | one to three text/choice questions, when supported |
| `update_plan` | `other`   | replace the current turn's execution plan          |

`write_file` and `edit` report what they changed as a diff in the tool call,
with a `git_patch` unless the change is too large to diff.

`write_file`, `edit` and `bash` run only after the user allows them through
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
[ADR 0014](docs/adr/0014-stream-agent-terminals.md). Automated tests validate the
wire protocol; actual T3 rendering remains pending [#38](https://github.com/anesthetised/acplause/issues/38).

File tools are confined to the session's `cwd` and `additionalDirectories`
with `os.Root`, which blocks escapes through `..` and symlinks. `bash` is not
sandboxed.

Read, write and edit operations accept regular files only; pipes and devices
are rejected. `write_file.content` and `edit.new_text` must be explicit
strings: an empty string is valid, but an omitted or null value is an error.

`AGENTS.md` at the root of the session's `cwd` is appended to the system
prompt, read once when the session is created. Only that file, with no
`CLAUDE.md` fallback and no parent directories. A missing file changes
nothing; one that is not a regular text file or exceeds 32 KiB is skipped
with a log line.

A session is titled after the first line of its first prompt. The title is
sent once, as a `session_info_update`, when that prompt is accepted, and is
returned by `session/list`. It is not replayed on resume: replay is
conversation history, and the title is metadata that `session/list` reports.

Prompts may embed resources (`session.prompt.embeddedContext`). A text
resource reaches the model as `<resource uri="...">` with its contents; a blob
resource is replaced by a one-line note naming its URI. The client still gets
every block echoed back unchanged.

In T3, select a model whose provider metadata explicitly lists image input,
then paste a screenshot into the composer or upload a PNG/JPEG attachment
and send the prompt. T3 supplies the image bytes over ACP; a file path in a
text prompt does not attach its pixels. Image-only prompts and multiple
images are accepted. A session without a textual title is named "Image prompt".
Actual T3 clipboard/upload interoperability is still awaiting the runtime
checks in [#38](https://github.com/anesthetised/acplause/issues/38); automated
tests cover the ACP-to-provider path. See [ADR 0010](docs/adr/0010-image-prompts.md).

Images are limited to 4 MiB decoded bytes and 16 million pixels each, four
images and 8 MiB decoded total per prompt. Invalid images, no model selection,
and models with text-only or missing image metadata fail before the prompt
is accepted. The agent never switches models on your behalf. Switching an
existing image conversation to a non-vision model sends explicit omission
markers for historical images; switching back restores their pixels in model
requests. Audio and other image formats are not supported.

Clients advertising `capabilities.elicitation.form: {}` enable the agent-owned
`ask_user` tool. It sends `elicitation/create` with one to three required string
fields, optionally with single-choice options. Only a complete accepted answer
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
[ADR 0011](docs/adr/0011-structured-user-questions.md) and the
[T3 form smoke procedure](docs/testing/t3-first-class-smoke.md). Actual T3 question
card acceptance remains pending [#38](https://github.com/anesthetised/acplause/issues/38).

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
actual T3 rendering remains pending the
[runtime acceptance procedure](docs/testing/t3-first-class-smoke.md) in
[#38](https://github.com/anesthetised/acplause/issues/38).

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

Resume replaces the entire MCP configuration; an omitted or empty list
removes all servers. Healthy unchanged connections are reused. Failed setup
preserves the previous configuration; later prompts wait for setup before
insertion. Changed/removed connections invalidate earlier model tool
bindings, so an old approval cannot invoke a replacement endpoint. Configs,
environment values, headers and upstream session IDs are kept in memory.
Tool results follow the normal conversation storage policy.

Setup allows 30 seconds, with at most four connections starting together;
discovery/refresh allows 15 seconds per server. `ACPLAUSE_MCP_TIMEOUT` starts
after permission, independently of the bash timeout. Cancellation stops local
waiting promptly and sends a best-effort MCP cancellation notification. A
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
Actual T3 screenshot interoperability remains a runtime gate in
[#38](https://github.com/anesthetised/acplause/issues/38).
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
prevents startup. A later save failure is logged to stderr; work continues
from memory, and unsaved changes may be missing after a restart.

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
