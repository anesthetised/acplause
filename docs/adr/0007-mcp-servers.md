# ADR 0007: Relay session-owned MCP tools over stdio and HTTP

- Status: accepted
- Date: 2026-10-19
- Issues: #12, #13, #14; image follow-up: #23
- Supersedes: ADR 0001's MCP exclusion
- ACP source: `testdata/schema/v2/schema.json` at `3be9cfc`
- MCP sources: [2025-11-25 lifecycle](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle),
  [transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports),
  [tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools),
  [cancellation](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation)

## Context

Before this change session setup accepted `mcpServers` but ignored it. Issues #13 and
#14 require one bridge for local processes and HTTP endpoints, including
stateful tools, cancellation and configuration changes on resume. Existing
ACP ordering, permission and history invariants must also hold for external
tools. Issue #23 will later add images returned by tools; collapsing MCP
results into a string in the transport would make that change unnecessarily
difficult.

## Decisions

1. **A bounded MCP client on the standard library.** Add `internal/mcp` for
   MCP messages, discovery, connection ownership and the two transports.
   Keep ACP framing and dispatch in `internal/jsonrpc`; share its marshaler
   and small wire helpers where applicable, not its ACP-specific connection
   lifecycle. There is no new runtime dependency. This is a tool client,
   not a general MCP server framework.

2. **An explicit protocol profile.** Request MCP `2025-11-25`; also accept
   `2025-06-18`. Reject other negotiated versions instead of assuming their
   semantics. Support initialize/initialized, ping, paginated tools/list,
   tools/call, tools/list_changed and cancellation. Advertise no roots,
   sampling, elicitation or MCP Tasks capabilities. Unknown notifications
   are ignored; unsupported server requests receive method-not-found.
   T3 asynchronous delegation is an ordinary tool, not the MCP Tasks feature.
   The newer `2026-07-28` protocol is a separate compatibility change, not
   implicitly enabled by a dependency upgrade.

3. **ACP shapes remain pinned.** Parse the schema's explicit `type: stdio`
   and `type: http` variants, with `name`, `command`/`url`, `args`, and the
   name/value arrays `env`/`headers`. Ignore unknown transport variants;
   validate known ones and reject duplicate configured server names before
   opening connections. Advertise `session.mcp.stdio: {}` and
   `session.mcp.http: {}` only when their implementation and e2e tests exist.
   Do not change the pinned schema in this work.

4. **Session ownership, not persisted credentials.** One ACP session owns
   its MCP connections and child processes. Use its `cwd` for stdio launch;
   execute the absolute `command` with `args` directly, without a shell.
   Inherit a documented minimal platform environment and overlay explicit
   `env`; provider credentials are not inherited implicitly. Configurations,
   environment values, HTTP headers and MCP session IDs remain in memory,
   outside the model prompt, logs and SQLite. MCP tool output is conversation
   data and follows the existing storage policy, including opaque references.

5. **Transactional resume and ordered setup.** The current request's entire
   `mcpServers` list replaces the desired configuration; omission/empty means
   none. Reuse unchanged healthy connections. Initialize and discover added
   or changed servers outside session locks, then publish the complete new
   configuration atomically. Failure closes the candidates and preserves the
   old configuration. Removed/replaced connections are revoked and closed.
   Reserve setup operations in ACP arrival order. Later prompts wait for the
   preceding setup result before insertion, without blocking the ACP reader;
   cancellation remains available while setup waits. Close/delete/ownership
   loss invalidate pending setup so it cannot publish resurrected resources.

6. **Stable names, immutable call bindings.** Generate model-safe names as
   `mcp_<server slug up to 8>_<tool slug up to 16>_<32 hex digest>`; the
   digest is derived from the unambiguous pair of original configured server
   name and original tool name. Names are at most 62 ASCII characters and
   stable across list order, pagination and reconnects. Check collisions
   against all registered names and reject, never silently overwrite.
   Retain original names for routing and titles. Each model request captures
   the MCP catalog and its connection/definition generations. A tool call
   may use only that binding: replacement must fail an obsolete call rather
   than route it to a newly configured server with the same alias.

7. **Whole catalogs and bounded refresh.** Read every tools/list page and
   retain descriptions, input schemas and annotations. Keep schemas as JSON;
   do not rebuild them into a supported-keyword subset. Duplicate tool names,
   repeated cursors or limits exceeded fail discovery explicitly. A supported
   list-changed notification marks the catalog dirty; one background refresh
   per server builds a replacement and atomically publishes it. Notifications
   coalesce. Failed refresh keeps the last complete catalog, reports the
   failure locally, and does not block unrelated servers. Removed or changed
   definitions invalidate not-yet-dispatched old bindings. Calls already
   dispatched to the same connection may complete under their old definition.

8. **Explicit permission for every external tool.** MCP annotations are
   untrusted hints. `readOnlyHint` may select a display kind; it never skips
   ACP permission. Reuse allow-once/reject-once and the existing
   `tool_call` subject, showing server/tool and the nested arguments. Only
   explicit allow executes. Launching a client-configured MCP server is part
   of session setup; individual model-requested tool calls require permission.
   Validate syntactic JSON-object arguments locally before asking; the server
   remains responsible for its schema's semantic validation. Do not add a
   partial JSON Schema validator or promote the test validator to runtime.

9. **Structured results before presentation.** The MCP boundary returns
   ordered content blocks, raw `structuredContent` and `isError`, not a
   preformatted string. Preserve JSON numbers, nesting and opaque references
   using `json.RawMessage`, without a float64 round trip. The agent projects
   that result separately into model text and ACP tool content/rawOutput.
   Text, embedded text resources and resource links have explicit textual
   representations; structured-only results include their JSON as text for
   the current model API and as an object/value in ACP rawOutput. Do not fetch
   returned links automatically or elevate server text to system instructions.

10. **Unsupported content is visible and bounded.** Until #23, image, audio
    and blob blocks become short markers naming the unsupported type and
    MIME type, never base64 text. Mixed results retain their usable text/JSON
    with that marker. Unsupported-only results are failed with an explanation
    that the remote operation may have completed; empty successful results
    remain successful. Unknown content variants are represented as unsupported
    rather than crashing parsing. Apply limits before decoding large bodies;
    oversized results fail explicitly without slicing structured JSON or
    pretending a side effect was rolled back.

11. **Cancellation and failure do not break tool history.** Each model tool
    call gets a result, including remaining calls in a cancelled batch. MCP
    `isError`, RPC errors, malformed arguments, unavailable servers, revoked
    bindings and timeouts become failed tool updates and model-visible
    results. Only cancellation of the turn itself ends it as `cancelled`.
    Local waiting stops promptly and sends best-effort
    `notifications/cancelled` for the exact MCP request ID; late responses
    cannot complete a subsequent call. Never automatically replay a
    tools/call after timeout, disconnect or ambiguous delivery.
    Crashed stdio processes restart on explicit resume. Expired HTTP sessions
    get one bounded reinitialization attempt for a future model round;
    unsuccessful recovery waits for resume rather than looping.

12. **Independent time budgets.** Setup has a 30-second total deadline;
    per-server discovery/refresh has 15 seconds. Tool calls have a ten-minute
    default deadline configured by `ACPLAUSE_MCP_TIMEOUT`; explicit `0`
    disables that tool deadline while preserving cancellation. The tool clock
    starts after permission and covers the entire call, including progress
    and SSE reconnection; progress does not extend it indefinitely. ACP
    permission waiting follows the existing cancellation policy. These limits
    are independent of `ACPLAUSE_BASH_TIMEOUT`.

13. **Both HTTP response modes, without replaying operations.** HTTP means
    Streamable HTTP: POST with JSON or SSE responses, optional GET event
    stream, negotiated protocol header, server session ID and best-effort
    DELETE on close. GET 405 is supported. Resume interrupted SSE streams only
    by GET with their Last-Event-ID, respecting retry and the original call
    deadline; never repeat the original POST. An interrupted call with no
    replay cursor fails with unknown outcome. On expired sessions, retire the
    old connection and reinitialize for future calls; do not reissue the
    failed operation. Legacy HTTP+SSE, OAuth and interactive authentication
    are outside this change. Explicit client headers are supported. Disable
    redirects so credentials cannot cross endpoints; allow configured local
    endpoints, which are needed for T3. A model cannot choose the endpoint.

14. **Bounded cleanup.** Close/delete/shutdown/lease loss stop new calls,
    cancel local waits and release all connections, including candidates not
    yet published. For stdio close stdin, allow one second, terminate, allow
    one second, then kill; on Unix include the owned process group and reap
    the child once. HTTP DELETE has its own two-second bound. Resource
    cleanup runs off the ACP reader and does not hold the session mutex.
    Other platforms retain their documented direct-child cleanup limitation.

15. **A deliberate seam for #23.** Keep transport decoding separate from
    `agent` result projection. #23 can add a validated image block projection,
    multimodal `llm.Message` content, capability checks, persistence and
    compaction policy without changing MCP routing or permission handling.
    Do not implement image decoding, storage or model request parts here.
    Images received before #23 are omitted under decision 10 and will not
    become retroactively available after an upgrade. Image support must never
    silently switch the user's selected model. User image/audio prompts and
    audio tool results remain separate work.

## Alternatives considered

- **Official Go MCP SDK.** Examined
  [v1.8.0](https://github.com/modelcontextprotocol/go-sdk/tree/v1.8.0), including
  its [module dependencies](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/go.mod),
  [transports](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/transport.go)
  and [result types](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/protocol.go).
  It would reduce protocol code, but adds a runtime dependency graph and
  capabilities beyond the selected profile. Raw JSON fidelity and process-group
  cleanup would still need an adapter. The current project constraint favors
  a narrow stdlib implementation. Revisit via an ADR if this profile expands;
  adding the SDK would require the dependency approval stated in AGENTS.md.
- **Extend the ACP connection into a universal transport.** Rejected: ACP's
  NDJSON writer, deferred replies and cancellation method have different
  ownership and flow from MCP HTTP. Keep the existing ACP read-loop invariant
  explicit instead of introducing transport conditions throughout it.
- **Flatten results at the transport.** Rejected: it loses structured-only
  output and makes #23 require changes to every transport.
- **Reimplement T3 orchestration in the agent.** Rejected: delegation,
  worktrees, scheduling, browser control and PR watching belong to T3. Opaque
  `taskId`, `childThreadId` and `secretRef` values pass through as data. A later
  T3 completion enters through the host's ordinary ACP prompt mechanism;
  arbitrary MCP notifications do not create user prompts.

## Consequences

- The implementation owns a small, explicit MCP compatibility surface and
  must test both wire transports rather than rely on SDK conformance.
- Existing JSON history/transcript storage can hold the initial projections;
  no storage migration or MCP credential persistence is needed for #12–14.
- Exact references are preserved through relay and untrimmed history.
  Historical context compaction still follows ADR 0006; this bridge does not
  promise permanent model memory of every old handle.
- README and AGENTS.md describe the implemented scope; ADR 0001's exclusion
  is superseded. Both transports are advertised and schema-validated in e2e.
- Actual T3 interoperability needs a smoke test through the built acplause
  binary. Harness-native T3 calls are not evidence that this bridge works.
  See [the smoke record](../testing/mcp-t3-smoke.md) and
  [the implementation plan](../../tasks/plan.md) for verification and #23.
