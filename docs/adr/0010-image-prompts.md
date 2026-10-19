# ADR 0010: Accept user image prompts, including T3 clipboard attachments

- Status: accepted; ACP prompt path implemented in #28, actual T3 smoke pending #38
- Date: 2026-10-19
- Issues: [#28](https://github.com/anesthetised/acplause/issues/28)
- Scope change: ADR 0001 text-only prompt scope for validated PNG/JPEG images
- Baseline: acplause `d9c99f1`; ACP `2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`

## Context

T3 can turn a pasted clipboard screenshot into an ACP image block. Before
#28, acplause omitted image capability and promptText skipped such blocks.
Tool images (#23) do not enable clipboard or uploaded user attachments.

## Decisions

1. Reuse ADR 0009's image validation, history and wire projection. This feature depends on its media/history foundation, not on MCP image tools being enabled. There is no clipboard API in acplause: T3 owns paste, file upload and attachment transport.

2. Once the complete path ships, advertise capabilities.session.prompt.image: {} during initialize. This is a process capability, not a per-model promise; the selected model is checked at prompt admission. No new method, dynamic initialize capability, or model substitution is introduced.

3. Validate known image blocks outside the session lock before insertion. At the ordered insertion boundary recheck the actual selected model. Reject invalid/oversized images, absent selection, or a text-only/unknown model with a clear -32602 before user_message, history or provider traffic. Preserve the existing -32800 cancellation rule. A queued prompt must not rely on the model selected when it was queued.

4. Keep incoming ACP blocks verbatim for the user's echo and replay, including unknown fields. Model projection uses validated image bytes separately from promptText; text/resource extraction remains unchanged. Image-only prompts are valid. Supply a bounded text label for model context and a stable 'Image prompt' session title if no textual title exists; do not use base64 or resource contents as the title.

5. Serialize user text followed by image_url parts using ADR 0009. Preserve image order relative to other images; ACP echo preserves the original full block order. Subsequent switches to non-vision models use explicit historical-image markers as defined there.

6. Test clipboard paste in actual T3 separately from fake-client e2e: paste a locally created PNG containing a random short visual marker and ask the model to identify it without exposing the answer in text. Record T3 version, agent commit, selected model and result. Test text-only-model rejection, multiple images, image-only input and restart/resume. Attachment thumbnail rendering alone is not success.

## Consequences

The capability unlocks T3 attachment submission, while unsupported model choices still fail explicitly. Images in prompts become persisted conversation data. No new dependency, OS clipboard access, OCR service or image-generation support is needed.

## Sources and execution

Wire shapes come only from the pinned schema and these pinned protocol pages:

- [initialization](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/initialization.mdx)
- [content](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/content.mdx)
- [prompt-lifecycle](https://github.com/agentclientprotocol/agent-client-protocol/blob/3be9cfc8ea2dfe5a28973b461fd956b4045f24d3/docs/protocol/v2/prompt-lifecycle.mdx)

T3 compatibility evidence: checkout `94285424c`, `packages/provider-acp/src/server/{adapter,runtimeModel,AcpSessionRuntime}.ts` and `docs/user/providers-acp.md`. This is client evidence, not a source of ACP wire shapes.

Execution plan: [prompts](../../tasks/t3/prompts.md). #28 implements admission,
projection, capability advertisement and restart/replay, with schema-valid e2e
and in-package coverage. Actual T3 clipboard and upload observations remain
unverified under [#38](https://github.com/anesthetised/acplause/issues/38);
automated tests are not evidence of the real-client gate.

