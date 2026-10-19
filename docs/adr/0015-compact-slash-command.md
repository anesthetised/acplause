# ADR 0015: Advertise and execute a deterministic /compact command

- Status: proposed; implementation contract prepared for the requested roadmap
- Date: 2026-10-19
- Issues: [#37](https://github.com/anesthetised/acplause/issues/37)
- Scope change: Extends ADR 0006 with user-triggered compaction; automatic compaction stays unchanged
- Baseline: acplause `d9c99f1`; ACP `2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`

## Context

T3 populates its provider slash menu from availableCommands. acplause has automatic compaction but no advertised user action to invoke it.

## Decisions

1. Initially advertise only compact, with no arguments, in session/new and session/resume. The availableCommands list is static, so do not emit available_commands_update without a real list change. T3's '$' skill naming is client behavior, not a reason to invent skills or commands.

2. Recognize only a prompt containing exactly one text block whose trimmed text is '/compact'. Mixed blocks, '/compact explanation', '/compactness' and unknown commands remain ordinary prompts. Parse structurally; never execute a shell command or treat slash text in an embedded resource as a command.

3. Use the existing queue and insertion boundary: accepted /compact is an ordinary echoed user message with messageId, followed by foreground compaction work on the session worker. Require a selected model. No provider work occurs in the ACP handler. Cancellation before insertion rejects; afterwards it ends once with idle(cancelled).

4. Add an explicit force path to existing compaction code. Summarize older history even below the automatic threshold, preserving the system and the command prompt, and using the same selected model, summary validation, round budget and ownership-checked transaction. Do not run the normal answer/tool loop afterwards. A known context window is required; otherwise return a clear agent message saying compaction was not performed. If nothing can be summarized, send a short no-op agent message and end normally.

5. After successful compaction, emit a short agent confirmation and the existing compaction_update lifecycle; failures after insertion use idle(error), not a rejected prompt. Replay retains the original full conversation. Summary calls remain counted under ACPLAUSE_MAX_ROUNDS and usage/cost.

6. Implement after image-aware history so manual compaction cannot serialize images into summary text. Do not add /clear, /reset, /plan, shell passthrough, skills discovery or a command framework.

## Consequences

A single real command is sufficient to exercise the standard discovery path and provide useful control. A small agent-owned dispatcher avoids making command execution depend on the model recognizing slash text.

## Sources and execution

Wire shapes come only from the pinned schema and these pinned protocol pages:

- [slash-commands](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/slash-commands.mdx)
- [prompt-lifecycle](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/prompt-lifecycle.mdx)

T3 compatibility evidence: checkout `94285424c`, `packages/provider-acp/src/server/{adapter,runtimeModel,AcpSessionRuntime}.ts` and `docs/user/providers-acp.md`. This is client evidence, not a source of ACP wire shapes.

Execution plan: [commands](../../tasks/t3/commands.md). The roadmap's request is to prepare this design, not implement it. Mark accepted when implementation is authorized; do not silently revise the contract in a small execution task.

