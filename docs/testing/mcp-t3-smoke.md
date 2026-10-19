# Real T3 MCP smoke — 2026-10-19

Result: passed through the built acplause ACP provider, including asynchronous
completion in a later turn and tools after an explicit `session/resume`.
No CLI fallback or harness-native tool call substituted for the child
provider's MCP calls.

## Runtime and configuration

- Build: `a0460f5` on `feat/mcp-servers`, Go `go1.27.2 darwin/arm64`.
  Subsequent commits add cleanup hardening, regression tests and docs.
- Host: T3 Code (Beta), bundle version `0.1.0`.
- ACP provider: `acpRegistry_openrouter`, model `openai/gpt-6.1-sol`, high
  reasoning. T3 launched the repository's acplause binary.
- ACP: pinned v2. MCP profile: requests `2025-11-25`, accepts that version or
  `2025-06-18`; the metadata trace did not capture the negotiated MCP reply.
- Transport observed in ACP setup: **stdio**, server name `t3-code`.
  No real-T3 HTTP interoperability is claimed; HTTP has local integration/e2e
  coverage.

The actual command and two arguments (confirmed from the owned child process):

```text
command: /Applications/T3 Code (Beta).app/Contents/MacOS/T3 Code (Beta)
args:
  /Applications/T3 Code (Beta).app/Contents/Resources/app.asar/apps/server/dist/bin.mjs
  acp-mcp-bridge
```

T3 supplied `ELECTRON_RUN_AS_NODE`, `T3_ACP_MCP_ENDPOINT` and
`T3_ACP_MCP_AUTHORIZATION` through the config's name/value `env` array.
Endpoint and authorization values were neither printed nor recorded.
There were no HTTP config headers. The same config shape was supplied on
resume. A temporary external wrapper forwarded ACP bytes unchanged and logged
only method/ID, server type/name/command, argument count, environment/header
names and tool/state transitions. It recorded no prompts, results or secret
values. The normal binary was rebuilt after this test.

## Observations

The successful smoke used request ID `acplause-mcp-real-smoke-20261019-4`.
Its ACP session was `c843a45d-af00-45a1-b6b2-dc728d40b803`.

1. `orchestrator_capabilities` was discovered through MCP and completed.
2. `preview_status` with `tabId: "acplause-smoke-missing-tab"` completed and
   returned `available: false`. This tested the textual browser operation;
   no page was opened and no screenshot/image behavior was claimed.
3. `delegate_task` ran asynchronously through MCP, targeting provider `codex`,
   model `gpt-6.1-sol`. The child was instructed only to return
   `ACPLAUSE_NESTED_MCP_SMOKE_OK`, with no tools or changes.
4. The automatic completion notification entered as a second ACP prompt on
   the same session. `task_status` returned `completed` and the exact marker.
   Browser status and capability discovery worked again in that later turn.
5. The idle smoke ACP process was stopped gracefully and its temporary
   wrapper removed. A follow-up to the same T3 child forced reconnection.
   The parent trace observed `initialize`, `session/new`, then
   **`session/resume` with the original ACP session ID and injected stdio
   config**, followed by `session/prompt`.
6. After that explicit resume, `orchestrator_capabilities`, `preview_status`
   and `task_status` all completed. The original nested task ID still returned
   the same completed result.

The opaque references survived tool output, later-turn history and resume:

```text
taskId:
node:delegated-task:command%3Amcp%3Aac0dc46f-a26b-4a07-bf11-a4cec769cb2b%3Adelegate-task%3Aacplause-nested-smoke-20261019-4
childThreadId:
thread:delegated-task:command%3Amcp%3Aac0dc46f-a26b-4a07-bf11-a4cec769cb2b%3Adelegate-task%3Aacplause-nested-smoke-20261019-4
```

## Initial failed attempts

Attempts 1–2 discovered tools successfully but the model supplied
`preview_status {"tabId":null}`. T3 rejected it with `-32602`, expecting a
string or omitted field. Attempt 3 similarly supplied `timeoutMs:null` to
delegation, which T3 rejected. These failures reached the model as failed
tool results; the bridge did not discard or rewrite explicit null values.
Using explicit valid optional values completed the smoke. This is a model/
upstream argument compatibility observation, not an automatic null-stripping
policy in acplause.

Image results remain deferred to #23. Automated fixtures check explicit
omission markers and that binary blocks do not enter model text or ACP replay.
