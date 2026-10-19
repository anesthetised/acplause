# AGENTS.md

Guidance for coding agents working on acplause, an ACP v2 agent in Go that
proxies an ACP client (editor) to an OpenAI-compatible Chat Completions API,
OpenRouter first. Read [README.md](README.md) for what it does and
[docs/adr](docs/adr) for decisions already made; do not relitigate them
without writing a new ADR.

## Commands

```sh
just check   # gofmt, go vet, staticcheck, go test -race ./..., go mod tidy -diff
just test -run TestName
just e2e -v
```

Run `just check` before handing work back. All of it must pass.

## Layout

```
cmd/acplause/      entry point; configuration from ACPLAUSE_* env vars
internal/jsonrpc/  NDJSON framing, dispatch, batches, single stdout writer
internal/acp/      ACP v2 wire types, only those acplause uses
internal/agent/    sessions, prompt queue, lifecycle, cancellation, tool loop
internal/llm/      Chat Completions client: SSE, retries, models list
internal/tools/    tools and the os.Root sandbox
internal/store/    SQLite sessions, migrations and ownership leases
e2e/               tests of the built binary over stdio
testdata/schema/v2 pinned ACP schema; see VERSION
```

## Sources of truth

- **Wire shapes come only from `testdata/schema/v2/schema.json`** and the ACP
  docs at the commit in `testdata/schema/v2/VERSION`. acp-bridge is a
  reference for behaviour (tool loop, limits, error classes), never for wire
  shapes.
- ACP v2 is a draft. Update the pinned schema in its own change, refresh
  `VERSION`, and fix whatever the e2e schema validation then reports.

## Invariants

- **stdout carries only protocol messages.** Log with `slog` to stderr. All
  output goes through `jsonrpc.Conn`'s single writer.
- **Handlers run on the read loop in arrival order and must not block.** Work
  that waits returns a `*jsonrpc.Reply` and settles it from a goroutine. This
  is what keeps `session/prompt` → `session/cancel` ordered.
- **The insertion boundary.** Before the user message is inserted,
  `session/prompt` fails with a JSON-RPC error (`-32800` when cancelled).
  After insertion it always succeeds with `messageId`; failures become
  `state_update {state: "idle", stopReason: "error", error}`.
- **Every turn ends with exactly one idle `state_update`**, `cancelled` when
  the turn's context was cancelled, whatever error surfaced.
- **State updates are sent under the session lock**, so resume snapshots and
  live changes cannot reorder.
- **Every tool call the model made gets a tool result in the history**, even
  when cancelled, or the next request is invalid.
- **Updates are patches.** Omitted fields mean "unchanged"; acplause never
  clears fields, so zero values are omitted.
- **Custom `MarshalJSON` methods use `jsonrpc.Marshal`**, which leaves `<`,
  `>` and `&` unescaped.
- **Forward compatibility.** Unknown fields and `_`-prefixed variants must not
  break parsing; prompt content blocks are echoed back verbatim.
- **Errors meant for the model are tool results**, not protocol errors:
  invalid tool arguments, unknown tools, sandbox violations and declined
  permission are reported as a `failed` tool call and sent back to the model.
- **Agent-to-client requests block**, so `jsonrpc.Conn.Call` is used only from
  session goroutines. Anything but an explicit allow is not permission.

## Code style

- Go standard library, plus `modernc.org/sqlite` for storage
  ([ADR 0004](docs/adr/0004-sqlite-storage.md)). The JSON Schema validator is
  used by tests alone. Ask before adding any other dependency.
- Prefer stdlib helpers (`cmp.Or`, `slices`, `maps`, `errors.AsType`) over
  hand-rolled ones.
- Doc comments on every exported identifier and on non-obvious unexported
  ones; explain why, not what. Match the surrounding density.
- Depend on small consumer-side interfaces (`agent.Model`, `agent.Client`)
  so packages can be tested with fakes.
- Keep `internal/acp` to types that are actually sent or read.

## Tests

- New behaviour needs a test. Protocol-visible behaviour needs an e2e test.
- The e2e harness validates every message the agent writes against the
  schema: the root schema, plus the specific `*Response` definition for
  results and the `*Notification` definition for notification params. A new
  outgoing message type is covered automatically; a violation fails the test.
- E2E tests allow permission requests automatically; pass `askingUser` to
  `start`/`started` to answer them with `permissionRequest` and `answer`.
- E2E tests script the LLM with `mockLLM.enqueue` (`reply`, `callTool`,
  `waitFor`, or a custom `script`) and consume output with `take`,
  `untilIdle` and `updateWhere`. Messages a `take` skips stay available.
- E2E tests run in parallel. Each has its own agent process, mock server and
  temp directories; never share them or reassign `agent.t`. Each agent gets
  its own data directory; use `withDataDir` explicitly for tests of shared
  storage or restarts.
- Unix-only tests (process groups, `syscall.Kill`) go in files built with
  `//go:build unix`.

## Out of scope

MCP relay, auth methods, image and audio prompts, non-OpenAI-compatible
backends. ACP v2 has no client `fs/*` or `terminal/*` methods. The roadmap
is tracked in [GitHub milestones](https://github.com/anesthetised/acplause/milestones).
