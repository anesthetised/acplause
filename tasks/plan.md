# Plan: first-class ACP v2 support in T3 Code

Status: planned; architecture and execution tasks prepared, no feature code changed.
Date: 2026-10-19. Baseline: acplause `d9c99f1`; T3 source evidence `94285424c`.
Tracker: [#25](https://github.com/anesthetised/acplause/issues/25). **GitHub Issues are the authoritative task list.**
The completed previous plan is preserved in [mcp-implemented.md](mcp-implemented.md).

## Objective and boundary

Make the existing ACP provider useful with T3's image attachments and browser
screenshots, question cards, task plans, Plan/Build toggle, live command output
and provider slash menu. Retain existing MCP, permissions, persistent sessions,
model selection and cancellation behavior.

Clipboard paste is part of user image prompts ([#28](https://github.com/anesthetised/acplause/issues/28)), not tool image
results ([#23](https://github.com/anesthetised/acplause/issues/23)). The shared image foundation enables both independently.
T3 owns clipboard capture; acplause consumes ACP image blocks.

The pinned wire source is `testdata/schema/v2/schema.json`, version
`2.0.0-alpha.9` at `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`.
No schema update is planned. T3 sources are compatibility evidence only:
`packages/provider-acp/src/server/adapter.ts`, `runtimeModel.ts`,
`AcpSessionRuntime.ts`, and `docs/user/providers-acp.md`.

## Architecture and execution plans

- [ADR 0009: Carry bounded image tool results through model history](../docs/adr/0009-image-tool-results.md) — [execution plan](t3/images.md).
- [ADR 0010: Accept user image prompts, including T3 clipboard attachments](../docs/adr/0010-image-prompts.md) — [execution plan](t3/prompts.md).
- [ADR 0011: Ask structured questions through ACP form elicitation](../docs/adr/0011-structured-user-questions.md) — [execution plan](t3/questions.md).
- [ADR 0012: Publish and persist item-based execution plans](../docs/adr/0012-structured-plans.md) — [execution plan](t3/plans.md).
- [ADR 0013: Expose a restrictive Plan mode through session configuration](../docs/adr/0013-plan-build-mode.md) — [execution plan](t3/modes.md).
- [ADR 0014: Stream agent-owned bash terminals with bounded replay](../docs/adr/0014-stream-agent-terminals.md) — [execution plan](t3/terminals.md).
- [ADR 0015: Advertise and execute a deterministic /compact command](../docs/adr/0015-compact-slash-command.md) — [execution plan](t3/commands.md).
- [Real T3 acceptance plan](t3/integration.md) — [#38](https://github.com/anesthetised/acplause/issues/38).

ADRs are proposed implementation contracts. Their supersession clauses take
effect when the corresponding feature ships; existing scope remains accurate
until then. Accept the relevant ADR when implementation is authorized, and make
scope updates to README/AGENTS in that feature's activation change.

## Ordered task index

1. [#26](https://github.com/anesthetised/acplause/issues/26) — Add bounded image values and Chat Completions image projection. Dependencies: none.
2. [#27](https://github.com/anesthetised/acplause/issues/27) — Make persistence and context compaction image-aware. Dependencies: [#26](https://github.com/anesthetised/acplause/issues/26).
3. [#23](https://github.com/anesthetised/acplause/issues/23) — Support image results from MCP tools. Dependencies: [#27](https://github.com/anesthetised/acplause/issues/27).
4. [#28](https://github.com/anesthetised/acplause/issues/28) — Accept user image prompts and T3 clipboard screenshots. Dependencies: [#27](https://github.com/anesthetised/acplause/issues/27).
5. [#29](https://github.com/anesthetised/acplause/issues/29) — Add opt-in cancellation for outbound ACP requests. Dependencies: none.
6. [#30](https://github.com/anesthetised/acplause/issues/30) — Expose ask_user through ACP form elicitation. Dependencies: [#29](https://github.com/anesthetised/acplause/issues/29).
7. [#31](https://github.com/anesthetised/acplause/issues/31) — Persist and replay ACP item-plan snapshots. Dependencies: none.
8. [#32](https://github.com/anesthetised/acplause/issues/32) — Expose update_plan as an agent tool. Dependencies: [#31](https://github.com/anesthetised/acplause/issues/31).
9. [#33](https://github.com/anesthetised/acplause/issues/33) — Persist session execution mode without changing current behavior. Dependencies: none.
10. [#34](https://github.com/anesthetised/acplause/issues/34) — Wire T3 Plan/Build configuration to enforced tool policy. Dependencies: [#33](https://github.com/anesthetised/acplause/issues/33), [#30](https://github.com/anesthetised/acplause/issues/30), [#32](https://github.com/anesthetised/acplause/issues/32).
11. [#35](https://github.com/anesthetised/acplause/issues/35) — Expose bounded streaming events from bash execution. Dependencies: none.
12. [#36](https://github.com/anesthetised/acplause/issues/36) — Publish and replay ACP terminal output for bash tools. Dependencies: [#35](https://github.com/anesthetised/acplause/issues/35).
13. [#37](https://github.com/anesthetised/acplause/issues/37) — Advertise /compact and execute it through the prompt queue. Dependencies: [#27](https://github.com/anesthetised/acplause/issues/27).
14. [#38](https://github.com/anesthetised/acplause/issues/38) — Verify first-class ACP workflows in actual T3 Code. Dependencies: [#23](https://github.com/anesthetised/acplause/issues/23), [#28](https://github.com/anesthetised/acplause/issues/28), [#30](https://github.com/anesthetised/acplause/issues/30), [#32](https://github.com/anesthetised/acplause/issues/32), [#34](https://github.com/anesthetised/acplause/issues/34), [#36](https://github.com/anesthetised/acplause/issues/36), [#37](https://github.com/anesthetised/acplause/issues/37).

This order is valid topologically. The clipboard task can be taken immediately
after HISTORY, without waiting for MCP activation. Independent foundations can
be implemented separately, but default to serial execution for a small model:
most activation tasks share agent/turn.go and session persistence. There is no
authorization here to launch agents automatically.

## Checkpoints

- After MEDIA/HISTORY: old text-only histories and upstream requests stay valid;
  image grouping, projection, persistence and compaction are covered before activation.
- After MCP/PROMPT: fake clients prove both image paths independently; clipboard
  acceptance remains an actual T3 gate.
- After CANCEL/ASK and PLANSTATE/PLAN: cancellation/replay invariants hold and
  native questions/plans have protocol e2e coverage.
- After MODESTATE/MODE: no Plan option was exposed without an enforced allowlist;
  no forbidden side effect can pass through permission approval.
- After SHELL/TERMINAL and COMMANDS: live output is bounded, pumps stop, forced
  compaction keeps the insertion boundary and no image bytes enter summary text.
- Final [#38](https://github.com/anesthetised/acplause/issues/38): all features run through the built ACP provider in actual T3.

## Instructions for a lightweight execution model

Give the model this prompt, replacing the issue number:

> Implement only issue #N from tasks/plan.md. Read AGENTS.md, the issue's ADR,
> and its execution-plan section first. Verify all dependency code exists in
> this checkout. Follow the numbered steps and fixed contracts. Add the named
> unit/integration/e2e cases, run the focused checks, then just check and measured
> coverage >=80%. Preserve all protocol, cancellation, replay and ownership
> invariants. Do not change the pinned schema, add dependencies, broaden scope,
> or declare a T3 smoke successful from mocks. Commit a completed tested slice
> locally; do not push unless the user explicitly requests it. Report changed
> behavior, tests, remaining gate and next issue. If a contract is inconsistent
> with code or a dependency is missing, report the specific blocker; do not invent
> a replacement architecture or silently reduce acceptance criteria.

Each issue is self-contained with its contract, files, dependency references,
steps and acceptance. Associated test files are expected in addition to the
listed production files. Internal groundwork stays inert until activation tests
pass. No test command is evidence until run against the implementation.

## Deferred boundaries

Native fork/rewind, active steering, native subagent lineage, auth, audio, general
MCP elicitation, image generation, WebP/GIF, interactive PTYs and arbitrary slash
commands are outside this plan. The pinned ACP v2 does not provide portable
fork/rewind/lineage methods; do not port v1 methods or T3 extensions as if they
were v2. Existing interrupt/restart steering and MCP delegation remain in scope
for regression testing. General tool_call_content_chunk and dynamic command
updates wait for a real producer; terminal_output_chunk and a static command list
cover the current requirements.

## Delivery of the planning work

The linked issues are created and existing #23 is updated without duplication.
The user authorized publication to main. Issues contain executable contracts
and link to the published ADRs and plans. Planning verification checks
links/dependencies, unchanged schema and code, and `just check`.
Runtime acceptance belongs to implementation.
