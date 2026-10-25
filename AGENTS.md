# AGENTS.md

Guidance for coding agents working on acplause, an ACP v2 agent in Go that
proxies an ACP client (editor) to an OpenAI-compatible Chat Completions API,
OpenRouter first. Read [README.md](README.md) for what it does and
[docs/adr](docs/adr) for decisions already made; do not relitigate them
without writing a new ADR.

## Communication and decisions

- Ask questions through `AskUserQuestion` (or the harness's equivalent
  structured user-input tool), so the user can answer in the interface.
  Include alternatives, trade-offs, and a recommendation in the question.
  Use plain chat questions only if no such tool is available, and explain
  that limitation.
- Consult the user before architectural decisions, dependency changes, or
  other consequential choices. Present alternatives, trade-offs, and a
  recommendation; challenge weak proposals. Obtain agreement before adopting
  a decision, marking an ADR accepted, or implementing it. This includes
  runtime, test, and development dependencies.
- Before non-trivial implementation, recommend a suitable model and reasoning
  effort. Recommend changes to the parent model or reasoning effort when
  justified; respect choices already made for the work. Prioritize accuracy
  over brevity.
- Propose parallel work when it is feasible and worthwhile, explaining the
  independent tasks, expected benefit, and integration risks. Suggest models
  and reasoning effort for delegated tasks as appropriate. Do not launch
  subagents or separate threads without user authorization.

## Repository workflow

- Track substantial changes in issues and separate branches.
- Only the parent agent commits, pushes, opens PRs, watches CI and merges. Use
  Conventional Commits and appropriate `Co-Authored-By` trailers. Commits and
  pushes of work branches need no separate approval. Opening PRs and merging
  require explicit user agreement covering those actions; approval to implement
  a change alone does not authorize either. PRs follow the repository's PR
  conventions and link their issues. After opening an approved PR, watch its CI
  and fix failures caused by the change. Merge with a merge commit only when
  the user has agreed to the merge and required checks have passed; passing CI
  alone does not authorize merging. Force-pushing, pushing directly to `main`
  and deleting branches still need an explicit user request.
- Preserve unrelated and uncommitted work. Treat files as shared state and
  assign non-overlapping edits to concurrent agents.
- Use CI, maintain documentation, add meaningful tests, and preserve required
  coverage. Follow the Commands and Tests sections below.
- Use meaningful, reproducible benchmarks where appropriate, especially for
  performance-sensitive changes and optimization claims. Choose representative
  workloads and metrics; do not substitute microbenchmarks for the behavior
  that matters. Before running benchmarks, agree with the user on workload and
  resource cost. Report relevant before/after results for performance changes,
  including measurement conditions and limitations.

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
internal/mcp/      session-owned MCP peers, discovery, stdio and HTTP
internal/media/    validated PNG/JPEG values with ADR 0009 bounds
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
- **Agent-to-client requests block**, so `jsonrpc.Conn.Call` and `CallCancellable`
  are used only from session goroutines. Permissions use `Call`; direct form
  elicitation uses `CallCancellable`. Anything but an explicit permission allow
  is not permission; accepted form answers never authorize tools.
- **MCP bindings belong to a model round.** Reconfiguration cannot redirect
  an old call or approval. Every MCP tool requires permission, including one
  with `readOnlyHint`; a lost response never causes a tools/call replay.
- **MCP configs stay in memory.** Never persist/log environment values,
  headers or upstream session IDs. Preserve raw JSON until result projection;
  validated PNG/JPEG tool images use typed history attachments (ADR 0009),
  never base64 in text, summaries or rawOutput. Other binary content is omitted.

## Code style

- Go standard library, plus `modernc.org/sqlite` for storage
  ([ADR 0004](docs/adr/0004-sqlite-storage.md)). The JSON Schema validator is
  used by tests alone. Dependency changes require the agreement described above.
- Prefer stdlib helpers (`cmp.Or`, `slices`, `maps`, `errors.AsType`) over
  hand-rolled ones.
- Doc comments on every exported identifier and on non-obvious unexported
  ones; explain why, not what. Match the surrounding density.
- Depend on small consumer-side interfaces (`agent.Model`, `agent.Client`)
  so packages can be tested with fakes.
- Keep `internal/acp` to types that are actually sent or read.

## Tests

- New behaviour needs a test. Protocol-visible behaviour needs an e2e test.
- Total Go statement coverage must stay at or above 80%, and CI fails below
  it. Check it with
  `just test -coverprofile=cover.out && go tool cover -func=cover.out | tail -1`.
  The e2e binary is not measured, so code reached only through e2e tests
  also needs in-package tests (`internal/agent/methods_test.go` drives the
  handlers over `jsonrpc.Conn`).
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
- Actual T3 acceptance is recorded in
  [docs/testing/t3-first-class-smoke.md](docs/testing/t3-first-class-smoke.md).
  Keep #38/#25 open while a required runtime gate fails or is unavailable;
  #70 tracks `/compact` discovery/dispatch and #71 tracks tool-image display.
  Test the built binary as a T3 ACP provider; parent tools, CLI fallbacks and
  automated fake clients do not establish child-provider interoperability.
  Record clipboard paste separately from file upload and tool images, using
  random pixel-only markers and secret-free metadata. Distinguish model
  delivery from client rendering; do not change wire shapes for a client gap.

## Out of scope

Provider sign-in and credential management are deliberately excluded:
`ACPLAUSE_API_KEY` is supplied by the client/launcher environment. Do not add
ACP auth methods, login/OAuth flows, token refresh or credential/keychain
storage; MCP uses client-supplied credentials. Rationale and trade-offs are in
[ADR 0023](docs/adr/0023-external-credential-management.md). This exclusion does
not change tool permissions or credential-handling protections.

Audio prompts/results and image formats beyond validated PNG/JPEG
prompts and tool results ([ADR 0009](docs/adr/0009-image-tool-results.md),
[ADR 0010](docs/adr/0010-image-prompts.md)),
non-OpenAI-compatible backends, legacy MCP HTTP+SSE and MCP server capabilities
beyond tools. ACP v2 has no
client `fs/*` or `terminal/*` methods. The roadmap
is tracked in [GitHub milestones](https://github.com/anesthetised/acplause/milestones).
