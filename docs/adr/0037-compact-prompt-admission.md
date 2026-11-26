# ADR 0037: Require `/compact` to be sent on its own

- Status: accepted
- Date: 2026-11-27
- Issue: [#227](https://github.com/anesthetised/acplause/issues/227)
- Supersedes: [ADR 0015](0015-compact-slash-command.md), decision 2 only
- Protocol: [ACP v2 beta.4 slash commands][commands] and [prompt insertion][insertion]

## Context

ACP beta.4 recognizes an advertised slash command at the start of the first
text block. It allows other content alongside a command and leaves its
interpretation to the agent. `/compact` is advertised without arguments and
performs only context compaction. Dropping attached content would lose user
input, while sending a recognized invocation to the model could surprise the
user with an ordinary answer or tool calls.

## Decisions

1. Find the first text block, skipping earlier non-text blocks. Recognize
   `/compact` at the start of that block when the command name ends at
   whitespace or at the end of the block. Leading whitespace, a slash command
   in a later text block, prefix aliases and unadvertised command names remain
   ordinary prompt content.
2. Accept a new invocation only when the prompt contains one text block and
   everything after `/compact` is whitespace or empty. If there are arguments
   or other blocks, return `-32602` before queueing or insertion and tell the
   user to send `/compact` by itself, then send text or attachments separately.
   General prompt block validation still applies. Pending and recorded keys
   retain their precedence under [ADR 0033](0033-prompt-idempotency-keys.md).
3. Retain the original content and message ID for accepted commands. The
   session worker inserts the user message once and runs the compaction
   lifecycle defined by [ADR 0015](0015-compact-slash-command.md). Stored
   history retains its recorded command classification and replays the
   original content.

## Consequences

Users who bundle input with `/compact` can split it into two prompts without
losing text or attachments. Older prompts keep their original meaning when a
session is resumed or compacted.

[commands]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/slash-commands.mdx#running-commands
[insertion]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/prompt-lifecycle.mdx#2-prompt-accepted
