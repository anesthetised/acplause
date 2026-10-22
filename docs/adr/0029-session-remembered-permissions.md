# ADR 0029: Remember shell permission decisions for the life of a session

- Status: accepted by the maintainer on 2026-10-22; implemented in #142
- Date: 2026-10-22
- Issues: [#141](https://github.com/anesthetised/acplause/issues/141), implementation [#142](https://github.com/anesthetised/acplause/issues/142)
- Scope change: Narrows ADR 0003 decision 2 and the "never introduce auto-allow state" sentence of ADR 0013
- Source: ACP `2.0.0-alpha.9` at `3be9cfc` (`testdata/schema/v2/VERSION`):
  `PermissionOptionKind` (`allow_once`, `allow_always`, `reject_once`,
  `reject_always`) and `protocol/v2/tool-calls.mdx#requesting-permission`

## Context

ADR 0003 offers `allow_once` and `reject_once` only and keeps no permission
state: "always" was treated as the client's auto-approval. The pinned schema
also defines `allow_always` and `reject_always`. The spec does not say who
remembers such a choice, so a client that is offered them may remember
nothing, and the user is asked again for every identical command.

The maintainer asked for a narrow, in-memory, per-session memory in the agent
(2026-10-22). This ADR defines it. It does not change what is asked for file
tools or MCP tools.

## Decision

1. **Who remembers.** The session, in process memory. Nothing is written to
   SQLite, logs or the wire. A process restart, `session/resume` in another
   process, session close or deletion forgets every decision. Decisions are
   never shared between sessions.
2. **Which operations.** Only `bash`. `write_file`, `edit`, `edit_batch` and
   every MCP tool keep offering `allow_once` and `reject_once` (ADR 0003, ADR
   0007): a file diff differs on each call, and every MCP call needs its own
   permission even with `readOnlyHint`.
3. **Key.** The exact command string, byte for byte, plus the session's
   absolute `cwd`. No prefixes, globs, normalization, tokenization or shell
   parsing. A different spelling is a different command. The key does not
   include the model, so a model change keeps decisions; the command is what
   the user saw.
4. **Options.** A `bash` request offers four options: `allow_once`,
   `allow_always`, `reject_once`, `reject_always`, with new stable option ids.
   When the session already holds the bound of 256 decisions the "always"
   options are not offered, and the request is the ADR 0003 one.
5. **Recording.** Only `outcome: selected` with the exact option id for an
   "always" kind records a decision. `cancelled`, errors, unknown outcomes or
   option ids, and a cancelled turn record nothing and are not permission.
   One key holds one decision; a later explicit choice replaces it.
6. **Use.** A remembered `allow` runs the call without a request, still
   reported `pending`→`in_progress` as in ADR 0003. A remembered `reject` ends
   it `failed` without a request, and the model gets a tool result that says
   the call was rejected by an earlier "always" choice in this session. No
   `requires_action` state is entered in either case.
7. **What memory does not bypass.** The memory is consulted after Plan mode,
   argument validation, root checks and the round's tool binding, never
   before. A call that would have been rejected without asking is rejected
   without consulting memory. Timeouts, process-group cancellation and
   output limits are unchanged.
8. **Lifecycle.** Decisions are cleared when the session's additional roots
   change or the session ends. Changing model, effort or mode does not clear
   them; Plan mode simply never reaches the memory.
9. **Audit and redaction.** The tool call update says the decision came from
   memory. Logs at any level record only that a remembered decision was used
   (key hash, never the command text or environment).
10. **No revocation UI.** The agent offers none: ACP defines no method for it
    and ADR 0001 forbids inventing one. The only revocation is ending the
    session.

## Alternatives rejected

- **Status quo (client-owned).** Safest and spec-pure, but identical commands
  re-prompt forever when the client does not remember.
- **Durable, cross-session storage.** Persists authority over restarts and
  needs revocation, expiry and migration; no demonstrated need.
- **File-path or tool-name keys.** Would let the model rewrite a file without
  the user seeing the diff.
- **Prefix or glob command keys.** Require a shell parser; one unsafe
  expansion becomes arbitrary execution.
- **Remembering MCP tool decisions.** Violates the "every MCP call asks"
  invariant.

## Consequences

- A user who chooses "allow always" for a command lets the model repeat that
  exact command, in that directory, for the rest of the session without a
  prompt. This is a deliberate widening of agent-held authority, limited to
  one literal string.
- Without revocation a mistaken "always" can only be undone by ending the
  session.
- Option lists differ between `bash` and the other tools.

## Verification (for #142)

In-package tests: each outcome and option id, bound reached, replacement,
reject precedence over a stale allow, Plan mode and invalid arguments before
memory, roots change, resume/restart forgetting, cancellation during the
request, concurrent sessions, MCP and file tools unchanged, no command text
in logs. E2E test over the built binary with schema validation of the
four-option request and of the no-request replay of a remembered decision.
