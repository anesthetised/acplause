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
  everything a command started, session list/resume/close.
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
| `ACPLAUSE_LOG_LEVEL`    | `info`                          | logs go to stderr                     |

Point your ACP v2 client at the `acplause` binary with these variables set.
The client receives the available models in `session/new` and selects one
with `session/set_config_option` before sending the first prompt. New
sessions show "Select a model" until then; prompts without a selection are
rejected. If the provider's model list cannot be loaded or contains no
models with tool support, session creation fails.

## Tools

| Tool          | Kind      |                                                   |
| ------------- | --------- | ------------------------------------------------- |
| `read_file`   | `read`    | text files up to 1 MiB                            |
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

Resuming a running session can replay partial messages and active tool calls.
Changes to additional directories apply to subsequent tool calls; a call
already running keeps its roots until it finishes.

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
