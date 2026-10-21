# Documentation

[acplause](../README.md) connects ACP v2 clients to an OpenAI-compatible Chat
Completions API. Start with [installation and client setup](../README.md#install).

## User guides

| Guide | Contents |
| --- | --- |
| [Configuration](configuration.md) | Environment variables, provider credentials, timeouts and turn budgets |
| [Setup diagnostics](troubleshooting.md) | Local checks and the optional online model-catalog probe |
| [Using acplause](usage.md) | Model selection, Plan/Build modes, prompt images, questions, plans and compaction |
| [Built-in tools](tools.md) | Search, edits, permissions, terminals, sandbox, project instructions and parallel reads |
| [MCP servers](mcp.md) | Client-supplied stdio/HTTP servers, permissions, lifecycle and tool images |
| [Sessions and storage](sessions.md) | Persistence, replay, multiple windows, ownership and save failures |

## Development and design

- [Development and testing](development.md): checks, CI, coverage and the e2e harness.
- [Releases](releases.md): manual publishing, Homebrew, provenance and recovery.
- [Architecture decisions](adr): scope, protocol and implementation decisions.
- [Coding-agent instructions](../AGENTS.md): repository rules and invariants.

## Runtime verification

- [First-class ACP workflows in T3](testing/t3-first-class-smoke.md): observed
  behavior and outstanding client compatibility gates, including `/compact`
  discovery/dispatch and tool-image display.
- [MCP in T3](testing/mcp-t3-smoke.md): real-client MCP smoke evidence.
- [Parallel reads benchmark](testing/parallel-reads-benchmark.md): local results
  for the opt-in read concurrency setting.
