# Plan: MCP tools over stdio and HTTP (#12, #13, #14)

Status: implemented; real T3 stdio smoke passed, including explicit resume.
Final verification and PR delivery are tracked below.
Date: 2026-10-19. Baseline: `4793b51`. Branch: `feat/mcp-servers`.

The user requested one branch and one PR for issues
[#12](https://github.com/anesthetised/acplause/issues/12),
[#13](https://github.com/anesthetised/acplause/issues/13), and
[#14](https://github.com/anesthetised/acplause/issues/14).
Architecture was prepared with Astra High; implementation continued after
the user switched to Sol High. [#23](https://github.com/anesthetised/acplause/issues/23)
is an architectural input and explicitly not part of this implementation.
Tasks remain tracked in those GitHub issues; this file is the execution and
verification checklist, not a request to create additional issues.

## Architecture contract

Read [accepted ADR 0007](../docs/adr/0007-mcp-servers.md) before implementation.
Its lifecycle, permissions, retry policy and result representation are the
contract. Do not replace them with an SDK or a narrower implementation just
to make tests pass. Any necessary design change must update the ADR and plan
explicitly. Existing ACP v2 wire types come only from the pinned schema.

### Package responsibilities

| Package | Responsibility |
| --- | --- |
| `internal/acp` | MCP config variants/capabilities from the pinned ACP schema; keep unknown variants forward-compatible. |
| `internal/mcp` (new) | Session-local server connections, bounded MCP RPC, stdio/HTTP, discovery, immutable catalogs and bound tool calls. No ACP notifications or model history. |
| `internal/agent` | Setup ordering, permissions, common tool lifecycle, MCP result projections, history/replay and ownership cleanup. |
| `internal/tools` | Existing built-in tools and roots. Preserve per-call root acquisition behavior. |
| `internal/llm` | Existing text-based Chat Completions wire format for this work. No MCP or ACP imports. |
| `internal/store` | Existing history/transcript persistence. No MCP configs or credentials and no new migration. |

Use small consumer-side contracts and real transports in integration tests.
Do not introduce a plugin framework, event bus or per-T3-tool special cases.

### Internal contracts

- `mcp.Config` is an internal normalized setup value. The agent converts
  schema-derived ACP configs into it. Equality uses transport, configured
  name and actual normalized command/arguments/environment or URL/headers;
  map ordering does not restart a server. Do not log the equality material.
- `mcp.Manager` owns the session's connections. Preparation builds a
  candidate set with rollback; publication swaps it; close is idempotent.
  These are concrete operations, not exported global registries.
- A catalog snapshot contains ordered tool descriptions/schemas and bindings
  to server connection identity plus definition revision. A binding cannot
  silently look up the current server by alias when Run is called.
- A raw MCP result retains ordered `[]json.RawMessage` content,
  `json.RawMessage` structuredContent (presence differs from omission), and
  `isError`. Decoding arbitrary JSON into float64 maps is not allowed on the
  relay path. Validate that arguments are a JSON object and send those bytes
  as an object, not a JSON string.
- The agent's prepared invocation has display metadata, an explicit
  `RequiresPermission`, optional preview, and a context-aware runner.
  Adapt built-ins to it without changing their semantics. This explicit flag
  decouples permission from the display `ToolKind` for MCP tools.
- Its result has model text, ACP content, raw JSON output and failure status.
  Built-in results can be adapted from `tools.Result`. A separate MCP
  projector converts raw blocks into that result. Do not force MCP through
  the current string-only `tools.Call.run` callback.
- The final prepared model round carries both `llm.Request` and the exact
  MCP catalog snapshot used to construct its tools. Refresh/compaction must
  rebuild them together. Do not put MCP bindings into the LLM wire types.

### Concurrency and failure rules

1. All network/process I/O and setup waits happen outside `Session.mu` and
   outside ACP handlers. Notifications only update bounded/coalesced work
   signals; they never synchronously list tools on the receive loop.
2. Reserve per-session lifecycle operations on the ACP read loop, then run
   them off-loop in arrival order. Cover concurrent resumes of a stored
   session too, before a local `Session` has been published. A mutex acquired
   later by unordered goroutines is insufficient for this ordering.
3. A later submitted prompt captures the preceding setup barrier. Wait for
   it before the insertion boundary, using a cancellable wait. A failed setup
   rejects dependent queued prompts before insertion; it must not silently
   execute them with the old requested environment. An already inserted turn
   may continue against connections that remain valid.
4. On successful reconfiguration, atomically install directories and MCP
   state. If either preparation fails, retain both old values. Revoke old
   bindings for changed/removed connections and cancel their local waits.
   Keep unchanged connections alive. Recheck binding validity after permission
   and atomically admit dispatch against revocation.
5. Cancellation reason matters: turn cancellation produces idle(cancelled);
   a revoked connection or tool deadline produces a failed tool result while
   the turn remains alive. When a permission wait ends for a reason other
   than turn cancellation, restore running before continuing the tool loop.
6. Close/delete mark the session unavailable on the reader immediately and
   cancel pending setup; cleanup is deferred. Shutdown tracks unpublished
   setup candidates as well as open sessions. Detecting lease loss during a
   write must revoke MCP work too, without waiting for the renewal timer.
7. A failed transport completes every pending request once. Request IDs are
   scoped to the connection and never reused there. A late response is ignored;
   it cannot enter model history directly. History is written only by the
   existing session worker.
8. Server failure does not take down unrelated servers or sessions. Future
   calls can use a newly initialized connection, but calls bound to the failed
   generation receive errors. Reconnection never repeats tools/call.
   A crashed stdio process restarts on explicit resume, not in a respawn loop.
   For an expired HTTP session, allow one bounded reinitialization attempt
   before the next model-round snapshot; on failure leave it unavailable until
   resume. Publish recovered connections as new generations.

### Transport details and bounds

- Implement the shared MCP peer privately in `internal/mcp`: a pending-call
  map, bounded delivery, request/response validation, server ping and
  notifications. Avoid broad changes to `internal/jsonrpc.Conn`.
- Stdio has one writer and one reader per owned process. Queued writes carry
  their request context so cancellation cannot send an abandoned call later.
  A stuck/partial write retires the connection and closes pipes. Stderr never
  reaches protocol stdout; keep diagnostic capture bounded and do not log
  configured credentials. Test a non-reading and a non-exiting server.
  Error labels identify the configured server, without serializing its config,
  authorization headers or URL query into diagnostics.
- MCP SSE parsing needs `id`, `retry`, multi-line `data`, comments and bounded
  frames. The existing LLM SSE parser intentionally discards IDs/retry; do
  not reuse it unchanged or refactor the LLM as part of this feature.
- HTTP sets Accept and Content-Type itself, uses negotiated MCP headers and
  handles JSON responses, POST SSE and optional GET SSE. Reject CR/LF header
  values and client overrides of protocol/framing headers. Explicit auth
  headers are passed only to the configured endpoint. Redirects are errors.
  Use request contexts, not a short global http.Client.Timeout for long streams.
- Reconnect only the interrupted stream using its own Last-Event-ID. Never
  mix GET notification-stream cursors with POST result-stream cursors. Without
  a cursor, an interrupted POST result is an ambiguous failure. Bound resume
  attempts by the original request deadline; cancellation interrupts backoff.
- Initial limits: 16 configured known servers, 100 pages and 1,024 tools per
  server, 8 MiB per JSON message/SSE event and per assembled server catalog,
  256 KiB per projected model/ACP tool result. Reject explicitly at a limit;
  never silently truncate JSON or hide a catalog page. Enforce body bounds
  before unmarshaling. No new user knobs except the MCP tool timeout yet.
- Setup budget: 30 seconds total, maximum four server preparations at once.
  Discovery/refresh: 15 seconds per server, one refresh in flight per server.
  Tool deadline: ten minutes by default, `ACPLAUSE_MCP_TIMEOUT=0` opts out.
  Cleanup: EOF 1s, TERM 1s, then KILL/reap; HTTP DELETE at most 2s.
- For stdio, inherit only HOME, PATH, USER, LOGNAME, TMPDIR/TMP/TEMP, LANG and
  LC_* plus Windows SYSTEMROOT/WINDIR/COMSPEC/PATHEXT/USERPROFILE/APPDATA/
  LOCALAPPDATA. Explicit config env overlays these. Tests prove that the
  agent's provider key is absent unless explicitly supplied by the client.

## Implementation sequence

Each slice starts with a focused failing test, stays compilable and is
committed separately on `feat/mcp-servers`. Use `just test -run ...` for
focused tests and `just check` for each completed implementation checkpoint.

### 1. Contract and wire admission (#12)

- [x] Accept ADR 0007 after architecture review; update ADR 0001's exclusion
  as superseded, and align README/AGENTS.md scope with what the final PR adds.
- [x] Add ACP MCP config/capability types using only the pinned schema.
  Preserve/ignore unknown variants; reject invalid known configs and duplicate
  names without launching anything. Keep capability advertisement gated by
  completion of the corresponding transport slice.
- [x] Add validated `ACPLAUSE_MCP_TIMEOUT` configuration and wire it through
  `agent.Config`; accept positive durations and explicit zero.

Likely files: `internal/acp/mcp.go`, `session.go`, `initialize.go`, their tests;
then `cmd/acplause/config.go`, `config_test.go`, `main.go` and `agent.Config`
in a separate small commit. Documentation changes are their own commit.

Verification: valid stdio/HTTP arrays, unknown types/fields, malformed config,
duplicate names, header validation, timeout parsing and unchanged empty-config
session behavior. Schema validation remains unchanged.

### 2. Stdio peer and discovery (#13)

- [x] Build the bounded peer and process transport with initialize/version
  negotiation, cancellation, orderly cleanup and server-request rejection.
- [x] Add full paginated discovery, stable aliases, raw argument/result
  preservation and list-changed refresh.
- [x] Use isolated fake server processes for launch cwd/args/env, ping,
  pagination, collisions, large JSON integer/opaque references, malformed
  messages, protocol rejection, duplicate/repeated cursors and crashes.

Likely files: `internal/mcp/client.go`, `stdio.go`, `process_unix.go`,
`process_other.go`, then `catalog.go` and their focused tests. Keep process
and catalog work in separate verified commits, not one large implementation.

Checkpoint: real stdio calls work under `go test -race`; cancellation and
cleanup do not leak a process, pipe or pending request.

### 3. One complete ACP-to-MCP turn (#13)

- [x] Add session manager ownership, model-round bindings and the prepared
  invocation adapter. Keep existing built-in permissions and root semantics.
- [x] Add the common tool lifecycle and separate model/ACP result projection.
  Test structured-only, text plus JSON, MCP isError/RPC errors, invalid args,
  denied permission, unknown tools, unsupported image/audio blocks and limits.
- [x] Add e2e ACP capability advertisement and a full stdio tool round.
  Every emitted message continues through the existing schema validator.

Likely files: `internal/agent/mcp.go`, `turn.go`, `compact.go`, `session.go`,
then `internal/agent` focused tests and `e2e/mcp_test.go`/fake fixture files.
Separate result projection from lifecycle changes in commits.

Verification fixtures include nested `schedule_task` arguments, a
`delegate_task` result with taskId/childThreadId, and a secretRef round trip.
Use arbitrary fixture names too so the bridge cannot accidentally depend on
T3 routing. At least one tool falsely claims readOnlyHint and is still gated.

### 4. Resume, cancellation and ownership races (#13)

- [x] Implement ordered setup barriers and atomic directory/MCP publication.
  Reuse unchanged healthy connections; rollback on partial setup failure.
- [x] Revoke changed/removed bindings and stop local waits on close/delete,
  lease loss and shutdown, including setup candidates.
- [x] Exercise resume while a model response streams, while permission is
  pending, during a tool call, and while another setup is still in progress.
- [x] Prove each cancelled multi-tool model response still has all tool
  results, and that a late response cannot corrupt the next prompt.

Likely files: `internal/agent/agent.go`, `session.go`, `persist.go`, `mcp.go`
and lifecycle tests. Add e2e cases separately in `e2e/mcp_lifecycle_test.go`
and Unix-only cleanup tests under a unix build tag.

Checkpoint: `just check`; no blocked ACP cancellation, new process after
close/delete, leaked child after takeover, or call rerouted across generations.

### 5. Streamable HTTP (#14)

- [x] Add HTTP transport and SSE parser to the same peer/manager contracts.
  Cover JSON and streaming responses, session IDs, protocol headers, explicit
  config headers, GET notifications, GET 405 and DELETE cleanup.
- [x] Cover SSE resumption by GET, retries/cursor isolation, deadline/cancel,
  404 session expiry and reinitialization for future requests. Prove a dropped
  response causes exactly one tools/call POST, even with an apparent
  idempotent/read-only hint.
- [x] Cover auth failures, forbidden redirect/credential forwarding, size
  bounds and unsupported legacy SSE endpoints.
- [x] Add ACP HTTP capability and e2e tests through local httptest servers.
  Run shared bridge behavior against both transports where appropriate.

Likely files: `internal/mcp/http.go`, `sse.go`, their tests, then capability
and `e2e/mcp_http_test.go` changes. No network access to external services in
automated tests.

Checkpoint: `just check` with race detection, including simultaneous sessions
using the same HTTP endpoint with isolated upstream MCP sessions.

### 6. Documentation, actual T3 smoke and one PR

- [x] Update README with supported MCP versions/transports, client-supplied
  setup examples, permissions, timeout/cleanup behavior, and unsupported
  content. AGENTS.md should include the new module and invariants.
- [x] Run the built acplause binary as the ACP agent inside T3, with actual
  injected MCP config. Record transport, command/args or endpoint, versions,
  build commit and results, redacting env/header secrets.
- [x] Discover orchestrator_capabilities; invoke a textual browser operation
  such as preview_status/preview_snapshot with includeImage=false; delegate
  a harmless asynchronous task; receive its completion in a later ACP turn;
  resume and call a tool again. If T3 provides HTTP, record this as HTTP
  coverage and do not claim real-T3 stdio interoperability.
- [x] Runtime gate available and passed: the actual injected config was
  stdio. The smoke used acplause as the ACP provider, not this harness's
  native tools. No draft-only runtime gate remains.
- [ ] Review diff, run final `just check`, push this branch and create one PR
  covering #12–14. Register its URL with T3 immediately. Do not merge it or
  include a closing reference for #23. Use closing references for other issues
  only when their acceptance gates are actually met.

## #23 boundary: what is deferred, and what must survive now

The current request implements no image support. The transport must still
recognize a result containing an image as a valid MCP result, keep the raw
ordered blocks until projection, and emit an explicit omission marker.
The following checks are part of #12–14:

- [x] Text plus image preserves text and reports the omitted image without
  leaking base64 into model text, ACP rawOutput, replay, logs or SQLite.
- [x] Unsupported-only output is distinguished from an empty success and
  from a server-reported isError; its remote side-effect uncertainty is clear.
- [x] raw structuredContent remains structured in ACP and intact in the
  current textual model projection, including nested arrays and opaque IDs.

#23 later owns a new ADR, bounded validated image decoding, model vision
capability checks, OpenAI-compatible multimodal request representation,
schema-valid ACP image content, and storage/replay/compaction policy. It may
need an attachment reference in durable history and a migration; no storage
format is promised here. Preserve this seam:

`MCP raw result -> validated content projection -> model history + ACP replay`

Specifically, #23 must replace the current assumptions in `llm.Message.Content`,
`toolResult`, `trimToolOutput`, `requestSize`, `summarize` and persistence
recovery. A new multimodal message decoder must still load old string content.
Do not estimate image tokens by counting base64 bytes, trim through a binary
part, silently switch models, or imply that tool images enable user image
prompts. Those are acceptance decisions for #23, not unfinished code in this PR.

## Architecture review scenarios

| Scenario | Required outcome |
| --- | --- |
| Two servers expose `read_file`; names contain punctuation/long prefixes | Unique stable model aliases; original tool names reach the correct servers. |
| Resume changes the endpoint during model streaming or permission | Old binding fails; no call reaches a replacement server on an earlier approval. |
| An in-flight remote tool is revoked or times out | Local wait ends; failed tool result; possible remote completion is disclosed; no automatic replay. |
| A second resume follows a slow first resume | Setup follows arrival order; the earlier completion cannot overwrite the later config. |
| Prompt follows resume, then cancel arrives | Prompt can be cancelled before insertion while the reader remains responsive. |
| One of several new servers fails startup | All new candidates close; prior config and directories remain intact. |
| Close/delete/takeover/shutdown races setup | No late publication, resurrected session, running child or active HTTP stream. |
| SSE response disconnects after executing a tool | Resume result by GET cursor only, or fail; tools/call POST count stays one. |
| Notifications arrive while no turn runs | Catalog refreshes without creating a prompt or modifying tool history. |
| Structured-only result contains nested IDs/large numbers | Exact values survive the bridge and are usable in a later call. |
| Tool returns text plus screenshot | Text works now, explicit image marker now; #23 adds projection later. |

## Verification record

- `just check` passes with race detection, vet, Staticcheck and tidy diff.
- `internal/mcp` tests cover raw JSON fidelity, aliases/pagination, refresh,
  invalid discovery/wire messages, minimal environment, blocked-write process
  group cleanup, HTTP JSON/SSE, GET cursor recovery, missing-cursor failure,
  one-POST delivery, expiry recovery, redirects, auth/setup errors and bounds.
- `internal/agent` tests cover text/structured/unsupported projections and
  size bounds, plus immediate lifetime cancellation on ownership loss.
- Schema-valid e2e tests cover actual stdio and HTTP servers, explicit
  permission despite readOnlyHint, nested objects and opaque references,
  cancellation/late results, independent timeout, resume replacement/rollback/
  ordering, queued-prompt cancellation and close/delete during stored setup.
  Existing built-in, persistence and compaction tests remain green.
- The real T3 test discovered orchestration, called textual browser status,
  delegated asynchronously, consumed completion in a later ACP turn and
  called tools again after an observed explicit `session/resume`.
  Configuration and opaque IDs are recorded in
  [the smoke record](../docs/testing/mcp-t3-smoke.md).

The test matrix above is the concrete implemented coverage; the scenario
lists in the original slices describe the review scope, not a claim that
every combination has a separate test. Images in #23 remain explicitly out
of scope. The original delivery remains one branch and one PR.
