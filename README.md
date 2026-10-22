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

Inspired by [acp-bridge](https://github.com/BlakeHung/acp-bridge), which serves
as a behavioral reference for the tool loop, limits and error handling.
acplause follows the pinned ACP v2 specification for its wire protocol.

acplause is experimental. Calendar versions identify releases; they do not
promise compatibility. Breaking changes are highlighted in release notes.

- ACP v2 only, validated against the pinned schema (`testdata/schema/v2`).
- Streaming text and reasoning, tool calls, usage and cost reporting.
- Live bash terminal output with bounded snapshots for session replay.
- Asks permission before changing files or running commands.
- Prompt queue, cancellation of model requests and Unix shell process groups,
  session list/resume/close/delete.
- Sessions survive restarts and can move between editor windows.
- Model and reasoning effort selection from OpenRouter's model list.
- PNG/JPEG prompt attachments and MCP tool images for models known to accept images.
- Session-owned MCP tools over stdio and Streamable HTTP.
- Structured user questions on clients advertising ACP form elicitation.
- Native ACP execution plans with persistent progress snapshots.
- Per-session Plan/Build modes with enforced tool restrictions.
- A discoverable `/compact` command to summarize older model context on demand.

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
OS (macOS or Linux) and architecture (arm64 or amd64) from
[GitHub Releases](https://github.com/anesthetised/acplause/releases).
Extract it and put `acplause` in a directory on your `PATH`. Release archives
include SHA256 checksums and GitHub artifact attestations. Prebuilt macOS
binaries are not Developer ID signed or notarized, so macOS may require
you to explicitly allow them to run.

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
