# ADR 0014: Stream agent-owned bash terminals with bounded replay

- Status: accepted; implemented with schema-validated tests; real-T3 validation pending (#38)
- Date: 2026-10-19
- Issues: [#35](https://github.com/anesthetised/acplause/issues/35), [#36](https://github.com/anesthetised/acplause/issues/36)
- Scope change: Extends bash presentation; preserves ADRs 0003 and 0008 execution/timeout policies
- Baseline: acplause `d9c99f1`; ACP `2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`

## Context

bash output is buffered until cmd.Run returns. T3 can already render terminal_update and terminal_output_chunk, so long builds can show useful progress without introducing client-owned terminals.

## Decisions

1. Each permitted bash invocation that actually starts gets a new terminalId. Send terminal_update with command/cwd, associate the tool with content:{type:'terminal',terminalId}, then stream terminal_output_chunk.data as independently base64-encoded bytes. No terminal/create, stdin, PTY, detached job, or client-side process ownership.

2. Introduce a small invocation-local shell observer contract in internal/tools, separate from ACP types. It reports start, bytes and exit, and keeps the current combined-output tool result. Existing callers without an observer preserve behavior. Denied permission and failed process start create no phantom running terminal.

3. The process pipe writer only copies bytes into bounded state and coalesces a wake-up; it does not write ACP or take Session.mu. A per-invocation pump publishes at most every 100ms, in chunks at most 16 KiB, through the normal single writer. Keep at most Options.OutputLimit bytes (default 50,000), matching the existing prefix-retention policy, count omitted bytes, and include an omission marker in the final replacement snapshot. Do not build an unbounded chunk queue.

4. Under the session lock, maintain one terminal transcript snapshot at its first-seen position and serialize live chunks with resume snapshots. Intermediate chunks are not individual stored rows. Preserve byte data even when UTF-8 splits across chunks. Final terminal_update replaces output with the bounded authoritative snapshot and supplies exitStatus before the tool completes.

5. Represent known exit code including zero with a pointer; use signal names on Unix and an empty exitStatus object when the process ended but details are unknown. Do not invent exit codes. Process recovery marks an unfinished terminal ended with unknown status, alongside the existing failed tool recovery.

6. Cancel, timeout, close/delete and ownership loss stop the process as today and stop/join its pump before terminal/tool finalization. No output is emitted after finalization and no goroutine outlives its session. Keep every sibling tool result after cancellation.

7. Persist bounded snapshots at existing save boundaries; streaming bytes can be lost on a crash just like assistant chunks. The model receives the same bounded final textual result as today, never base64 terminal bytes. General tool_call_content_chunk support is deferred until a non-terminal producer needs it.

## Consequences

T3 displays command progress and exit status while acplause continues to own process groups and cancellation. Output caps deliberately bound replay size; this is not an interactive terminal feature.

## Sources and execution

Wire shapes come only from the pinned schema and these pinned protocol pages:

- [tool-calls](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/tool-calls.mdx)
- [prompt-lifecycle](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/prompt-lifecycle.mdx)

T3 compatibility evidence: checkout `94285424c`, `packages/provider-acp/src/server/{adapter,runtimeModel,AcpSessionRuntime}.ts` and `docs/user/providers-acp.md`. This is client evidence, not a source of ACP wire shapes.

Execution plan: [terminals](../../tasks/t3/terminals.md). Issues #35 and #36
implement this contract. Automated tests cover live binary output, bounded
replacement and replay, simultaneous sessions, slow/failing publication,
permission rejection, failed starts, cancellation, timeout, close/delete and
crash recovery. The actual T3 client gate remains pending under #38;
fake-client tests do not satisfy it.

