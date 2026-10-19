# acplause

[![CI](https://github.com/anesthetised/acplause/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/anesthetised/acplause/actions/workflows/ci.yml)
[![Go coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/anesthetised/acplause/badges/coverage.json)](https://github.com/anesthetised/acplause/actions/workflows/ci.yml)

*Don't hold your acplause — it streams.*

acplause is an [Agent Client Protocol](https://agentclientprotocol.com) v2
agent. Your editor talks to it over stdio; it thinks with any
OpenAI-compatible Chat Completions API, [OpenRouter](https://openrouter.ai)
first, and works in your workspace through tools.

- ACP v2 only, validated against the pinned schema (`testdata/schema/v2`).
- Streaming text and reasoning, tool calls, usage and cost reporting.
- Asks permission before changing files or running commands.
- Prompt queue, cancellation that aborts the HTTP stream and kills
  everything a command started, session list/resume/close/delete.
- Sessions survive restarts and can move between editor windows.
- Model and reasoning effort selection from OpenRouter's model list.
- Session-owned MCP tools over stdio and Streamable HTTP.

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

## Tools

| Tool          | Kind      |                                                   |
| ------------- | --------- | ------------------------------------------------- |
| `read_file`   | `read`    | text files up to 1 MiB; `offset`, `limit` lines   |
| `list_dir`    | `read`    | tree, 3 levels, 200 entries                       |
| `search_code` | `search`  | plain-text search, 50 matches                     |
| `write_file`  | `edit`    | create or overwrite, up to 5 MiB                  |
| `edit`        | `edit`    | replace one exact occurrence                      |
| `bash`        | `execute` | `bash -c` in the working directory, output capped |

`write_file` and `edit` report what they changed as a diff in the tool call,
with a `git_patch` unless the change is too large to diff.

`write_file`, `edit` and `bash` run only after the user allows them through
`session/request_permission`, with the diff or command shown in the request.
acplause offers "allow once" and "reject once"; anything else, including a
cancelled or unrecognised answer, is a rejection, which the model is told
about. Automatic approval is the client's setting. See
[ADR 0003](docs/adr/0003-permissions.md).

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
every block echoed back unchanged. Images and audio are not supported.

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
per projected result; exceeding a limit fails explicitly.

Text, embedded text resources and resource links are supported. Image, audio
and blob results produce omission markers; unsupported-only output fails
with a possible-completion warning. Binary payloads are not stored or sent to
the model. Image projection is deferred to [#23](https://github.com/anesthetised/acplause/issues/23).
See [ADR 0007](docs/adr/0007-mcp-servers.md) and the
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
model and effort selections, and accumulated cost. See
[ADR 0004](docs/adr/0004-sqlite-storage.md).

`session/list` includes stored sessions from every process using that data
directory. `session/resume` restores their conversation history and settings.
`session/close` cancels work and releases the session in this process; its
history stays listed and can be resumed. `session/delete` cancels local work
and removes the session and its stored history. Deleting an unknown or
already deleted session succeeds. Nothing expires automatically.

Two editor windows can work on different sessions at the same time. Resuming
or deleting a session owned by another window is allowed while it is idle,
and refused while it is working or waiting for permission. After a takeover
or deletion, the old window loses access at its next write or lease renewal
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
