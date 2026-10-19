# ADR 0001: Scope and baseline decisions

- Status: accepted
- Date: 2026-10-18
- Source: ACP spec at `3be9cfc` (`testdata/schema/v2/VERSION`)

## Context

acplause is an ACP agent that proxies an editor (ACP client) to an
OpenAI-compatible Chat Completions API, primarily OpenRouter. ACP v2 is a
draft; the schema is pinned and every outgoing message is validated against
it in tests.

## Decisions

1. **ACP v2 only.** `initialize` with `protocolVersion < 2` fails with
   `-32022` and `data: {requested, supported: [2]}`. Higher versions select 2.
   Missing or non-integer versions are `-32602`.
2. **One backend:** OpenAI-compatible `/chat/completions` over SSE.
3. **Go + stdlib.** The only third-party module is the JSON Schema validator,
   used by tests.
4. **Own protocol types** in `internal/acp`, written from the schema, covering
   only what acplause sends or reads. No third-party ACP SDK.
5. **JSON-RPC batches are supported** (the spec requires accepting batch
   arrays on stdio). Entries are admitted in array order; the response array
   is written once every request in the batch is answered.
6. **`session.additionalDirectories` is advertised**, and the root set
   `[cwd, ...additionalDirectories]` is the sandbox for file tools.
7. **`session/list` paginates.** `cursor` is an opaque token; `nextCursor` is
   present only when another page exists; an invalid cursor is `-32602`.
8. **No `config_option_update` for client-initiated changes.** The
   `session/set_config_option` response already carries the complete option
   list, including dependent changes (e.g. `reasoning_effort` appearing when
   the model supports reasoning). The notification is reserved for changes the
   agent makes on its own.
9. **Insertion boundary.** Before the user message is inserted, `session/prompt`
   fails with a JSON-RPC error (`-32800` on cancellation). After insertion it
   always succeeds with `messageId`; failures are reported as
   `state_update {state: "idle", stopReason: "error", error}`.
10. **Request handlers run on the read loop in arrival order** and must not
    block; long work answers through a deferred reply. This keeps
    `session/prompt` → `session/cancel` ordering intact.

## Consequences

- Schema updates land as separate PRs that refresh `testdata/schema/v2` and
  re-run validation.
- MCP exclusion is superseded by [ADR 0007](0007-mcp-servers.md): session-owned
  stdio and Streamable HTTP tools are supported and advertised.
- Prompts support baseline `text` and `resource_link`, embedded resources,
  and validated PNG/JPEG images ([ADR 0010](0010-image-prompts.md)).
