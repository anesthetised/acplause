# ADR 0009: Carry bounded image tool results through model history

- Status: proposed; implementation contract prepared for the requested roadmap
- Date: 2026-10-19
- Issues: [#26](https://github.com/anesthetised/acplause/issues/26), [#27](https://github.com/anesthetised/acplause/issues/27), [#23](https://github.com/anesthetised/acplause/issues/23)
- Scope change: ADR 0007 binary-image omission, and ADR 0006 image budgeting/summary projection, when implemented
- Baseline: acplause `d9c99f1`; ACP `2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`

## Context

T3 browser tools return screenshots. The MCP transport accepts them, but the current projector drops binary content and llm.Message stores only text. Tool-image support must preserve every tool result and work with ordinary Chat Completions endpoints.

## Decisions

1. Use a shared internal/media package for validated immutable PNG/JPEG values. Accept at most 4 MiB decoded bytes per image, 16 million pixels, four images and 8 MiB decoded bytes per individual prompt/tool result. Check encoded length before decoding, MIME against the decoded format, dimensions with image.DecodeConfig, then full decode. Use stdlib image/png and image/jpeg; reject other formats explicitly. Never fetch image URLs or file paths. Existing 8 MiB MCP frame limits still apply before projection; these limits do not promise every maximum-size combination fits a transport frame.

2. Keep llm.Message.Content as text for compatibility and add typed image attachments in a reserved, optional persisted field. Old JSON rows without it still load. Attach tool images to the existing tool result with its tool_call_id; do not invent another user turn in canonical history. Define separate upstream wire-message types in internal/llm so internal attachment fields never leak to the API.

3. At Chat Completions serialization, emit all sibling role:tool results first, then one synthetic role:user message containing text labels with the originating tool_call_id and image_url data URLs. The label identifies tool output as untrusted data, not a new user instruction. User image attachments serialize in their own user message. Use this single portable representation; do not assume image parts in role:tool are accepted by every endpoint. A real OpenRouter vision smoke must verify the chosen representation before claiming interoperability.

4. Read architecture.input_modalities from /models; only an explicit image entry means known vision support. Missing metadata is unknown, not permission to guess. Never switch models. For text-only/unknown models, retain usable textual tool output and explicit image-omission markers; image-only results fail with a possible-completion warning. Images already retained in history are replaced with explicit markers only in a non-vision request projection, leaving canonical history/replay intact.

5. Persist validated images inline in existing JSON history/transcript rows initially. This needs no attachment table, external files, new dependency, or SQL migration. ACP emits image content in tool_call_update; rawOutput carries sanitized text/structured output and image metadata, not a second copy of base64. Keep binary data out of logs and ordinary model text. Full replay retains images until session deletion, like other history; document disk growth and lack of a session-wide storage quota.

6. Request budgeting excludes base64 bytes and reserves 8192 estimated tokens per retained image plus normal text overhead. This is an explicit heuristic, not a provider-independent token guarantee. Cap images in each model request at four and 8 MiB decoded total. Remove oldest unprotected images from model context with visible omission markers until both count/byte limits and the context estimate fit. Protect the latest real user prompt and latest complete tool exchange; fail explicitly if protected content cannot fit. Do not mutate replay, split image parts, or separate tool calls from their results.

7. Summary input replaces image bytes with source/MIME/dimension/omission descriptors, never JSON-encoded base64. Preserve surrounding textual observations; do not imply the summary saw the pixels. The protected recent content remains available to the vision model. Keep compaction cancellation, ownership checks and atomic history/transcript persistence from ADR 0006.

8. Separate validation, image-aware history, and MCP activation into dependency-ordered issues. Do not activate image projection until storage, request grouping and compaction tests pass. User image prompts are ADR 0010; audio, WebP/GIF, arbitrary blobs and MCP elicitation remain out of scope.

## Consequences

Inline storage deliberately trades disk duplication for a small implementation and existing transactional guarantees. Synthetic image messages require provider smoke coverage and must not affect latest-user tracking, titles or turn grouping. Explicit format and request bounds may reject some screenshots; a later ADR can expand formats or introduce an attachment store.

## Sources and execution

Wire shapes come only from the pinned schema and these pinned protocol pages:

- [content](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/content.mdx)
- [tool-calls](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/tool-calls.mdx)
- [prompt-lifecycle](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/prompt-lifecycle.mdx)

OpenRouter input format: https://openrouter.ai/docs/guides/overview/multimodal/image-understanding
Model metadata: https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties

T3 compatibility evidence: checkout `94285424c`, `packages/provider-acp/src/server/{adapter,runtimeModel,AcpSessionRuntime}.ts` and `docs/user/providers-acp.md`. This is client evidence, not a source of ACP wire shapes.

Execution plan: [images](../../tasks/t3/images.md). The roadmap's request is to prepare this design, not implement it. Mark accepted when implementation is authorized; do not silently revise the contract in a small execution task.

