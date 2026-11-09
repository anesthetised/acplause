<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/acplause-dark.svg">
    <img src="docs/assets/acplause-light.svg" alt="acplause, two clapping hands on an orange tile" width="128">
  </picture>
</p>

<h1 align="center">acplause</h1>

<p align="center"><em>Don't hold your acplause — it streams.</em></p>

<p align="center">
  <a href="https://github.com/anesthetised/acplause/actions/workflows/ci.yml"><img src="https://github.com/anesthetised/acplause/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://github.com/anesthetised/acplause/actions/workflows/ci.yml"><img src="https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/anesthetised/acplause/badges/coverage.json" alt="Go coverage"></a>
</p>

acplause is an [Agent Client Protocol](https://agentclientprotocol.com) v2
agent. Your editor talks to it over stdio; it thinks with any
OpenAI-compatible Chat Completions API, [OpenRouter](https://openrouter.ai)
first, and works in your workspace through tools.

Inspired by [acp-bridge](https://github.com/BlakeHung/acp-bridge), which serves
as a behavioral reference for the tool loop, limits and error handling.
acplause follows the pinned ACP v2 specification for its wire protocol.

> **Status:** experimental. Calendar versions identify releases; they do not
> promise compatibility. Breaking changes are highlighted in release notes.

## Highlights

- **ACP v2 only.** Wire shapes follow the pinned schema
  (`testdata/schema/v2`), and tests validate every message against it.
- **Streams everything.** Text and reasoning, tool calls, usage and cost
  reporting, and live shell terminal output with bounded snapshots for
  session replay.
- **Asks first.** Permission before changing files or running commands, and
  per-session Plan/Build modes with enforced tool restrictions.
- **Sessions that last.** Prompt queue, cancellation of model requests and
  owned shell processes, session list/resume/close/delete. Sessions survive
  restarts and can move between editor windows.
- **Your model, your effort.** Model and reasoning effort selection from
  OpenRouter's model list, and a discoverable `/compact` command to summarize
  older model context on demand.
- **MCP and images.** Session-owned MCP tools over stdio and Streamable HTTP;
  PNG/JPEG prompt attachments and MCP tool images for models known to accept
  images.
- **Talks back.** Structured user questions on clients advertising ACP form
  elicitation, and native ACP execution plans with persistent progress
  snapshots.

## Install

### Homebrew

```sh
brew install anesthetised/tap/acplause
```

The formula builds the tagged source on your machine, installing Go as a
build dependency. On macOS, local compilation lets us distribute acplause
without a paid Apple Developer ID certificate or an install hook that removes
quarantine from downloaded binaries. The trade-off is a slower first install,
including compilation of the SQLite dependency.

### Release archives

For a ready-to-run binary without installing Go, download the archive for your
OS (macOS, Linux or Windows) and architecture (arm64 or amd64) from
[GitHub Releases](https://github.com/anesthetised/acplause/releases).
Extract it and put `acplause` in a directory on your `PATH`. Release archives
include SHA256 checksums and GitHub artifact attestations. Prebuilt macOS
binaries are not Developer ID signed or notarized, so macOS may require
you to explicitly allow them to run.

On Windows 10/11, use the Windows ZIP archive and put `acplause.exe` on your
`PATH`, or configure your ACP client with its full path. Commands use the
built-in Windows PowerShell 5.1; Git Bash, WSL and PowerShell 7 are not required.
For example, from PowerShell:

```powershell
.\acplause.exe --version
.\acplause.exe doctor
```

The client must supply `ACPLAUSE_API_KEY` in the environment used to launch
`acplause.exe`. See [shell behavior and cleanup](docs/tools.md#shell-terminals).

### Go

If you already have Go installed:

```sh
go install github.com/anesthetised/acplause/cmd/acplause@latest
```

## Configure

Configure your ACP v2 client to launch `acplause` with `ACPLAUSE_API_KEY` set
to your provider key. The default provider is OpenRouter; set
`ACPLAUSE_BASE_URL` for another OpenAI-compatible API root. Without a key,
acplause sends no `Authorization` header, supporting compatible local providers.

Check setup from an environment with the same variables as the client:

```sh
acplause --version       # version and source commit
acplause doctor          # local configuration and data-directory checks
acplause doctor --online # also check the provider's model catalog
```

Select a model in the client before sending the first prompt. New sessions
reuse your last model and reasoning effort; acplause never picks a substitute.
See [configuration](docs/configuration.md), [setup diagnostics](docs/troubleshooting.md)
and the [usage guide](docs/usage.md).

## Documentation

- [Using acplause](docs/usage.md): models, Plan/Build, images, questions, plans and compaction.
- [Built-in tools](docs/tools.md): search, edits, permissions, terminals and project instructions.
- [MCP servers](docs/mcp.md): stdio/HTTP setup, lifecycle and tool images.
- [Sessions and storage](docs/sessions.md): resume, multiple windows and persistence.
- [All documentation](docs/README.md): guides, architecture decisions and runtime verification.

ACP v2 is a draft. Actual T3 compatibility and known client gaps are recorded
in the [runtime smoke record](docs/testing/t3-first-class-smoke.md).

## Develop

```sh
just check   # gofmt, vet, staticcheck, tests with -race, go mod tidy
just e2e -v  # end-to-end tests only
```

See [development and testing](docs/development.md) and [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE)
