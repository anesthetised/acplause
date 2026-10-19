# acplause

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

## Install

```sh
go install github.com/anesthetised/acplause/cmd/acplause@latest
```

## Configure

| Variable                | Default                         |                                       |
| ----------------------- | ------------------------------- | ------------------------------------- |
| `ACPLAUSE_API_KEY`      | `$OPENROUTER_API_KEY`           | API key                               |
| `ACPLAUSE_BASE_URL`     | `https://openrouter.ai/api/v1`  | any OpenAI-compatible API root        |
| `ACPLAUSE_MAX_ROUNDS`   | `25`                            | model requests per turn               |
| `ACPLAUSE_BASH_TIMEOUT` | `2m`                            | timeout of the `bash` tool            |
| `ACPLAUSE_DATA_DIR`    | `~/.acplause`                    | session database directory            |
| `ACPLAUSE_LOG_LEVEL`    | `info`                          | logs go to stderr                     |

Point your ACP v2 client at the `acplause` binary with these variables set.
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

The end-to-end tests in `e2e/` build the binary, drive it over stdio against
a scripted SSE server and validate every message it writes against the ACP
schema. Decisions are recorded in [`docs/adr`](docs/adr).

Notes for coding agents are in [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE)
