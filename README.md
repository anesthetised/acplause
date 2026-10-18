# acplause

*Don't hold your acplause — it streams.*

acplause is an [Agent Client Protocol](https://agentclientprotocol.com) v2
agent. Your editor talks to it over stdio; it thinks with any
OpenAI-compatible Chat Completions API, [OpenRouter](https://openrouter.ai)
first, and works in your workspace through tools.

- ACP v2 only, validated against the pinned schema (`testdata/schema/v2`).
- Streaming text and reasoning, tool calls, usage and cost reporting.
- Prompt queue, cancellation that aborts the HTTP stream and kills
  everything a command started, session list/resume/close.
- Model and reasoning effort selection from OpenRouter's model list.
- Go standard library only.

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

File tools are confined to the session's `cwd` and `additionalDirectories`
with `os.Root`, which blocks escapes through `..` and symlinks. `bash` is not
sandboxed.

## Develop

```sh
just check   # gofmt, vet, staticcheck, tests with -race, go mod tidy
just e2e -v  # end-to-end tests only
```

The end-to-end tests in `e2e/` build the binary, drive it over stdio against
a scripted SSE server and validate every message it writes against the ACP
schema. Decisions are recorded in [`docs/adr`](docs/adr).

Notes for coding agents are in [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE)
