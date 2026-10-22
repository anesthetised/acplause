# ADR 0003: Ask permission before changing files or running commands

- Status: accepted; decision 2 narrowed for `bash` by [ADR 0029](0029-session-remembered-permissions.md)
- Date: 2026-10-19
- Issue: #3
- Source: ACP spec at `3be9cfc` (`testdata/schema/v2/VERSION`):
  `protocol/v2/tool-calls.mdx#requesting-permission`,
  `protocol/v2/prompt-lifecycle.mdx#session-states` and `#cancellation`

## Context

acplause runs `write_file`, `edit` and `bash` without confirmation; `bash` is
not sandboxed. ACP lets the agent ask: "The Agent MAY request permission from
the user before proceeding with an operation" through
`session/request_permission`, a baseline client method with no capability.
Policy belongs to the client: "Clients MAY automatically allow or reject
permission requests according to the user settings." Permission modes shown
in the config option docs (`ask`, `brave_mode`) are examples, not
requirements.

## Decisions

1. **Ask before `write_file`, `edit` and `bash`.** Read, list and search
   tools never ask. Calls that would fail anyway, invalid arguments or an
   `edit` whose text is not found, fail without asking.
2. **Offer `allow_once` and `reject_once` only.** "Always" is the client's
   auto-approval; acplause keeps no permission state and has no permission
   modes of its own.
3. **Subjects.** `bash` uses `type: "command"` with the command, the
   session's absolute `cwd` and the `toolCallId`. File tools use
   `type: "tool_call"` with the pending update, including the diff the call
   would apply (#2). The request `title` is permission copy and does not
   replace the tool call's title.
4. **Tool call status.** The call is reported `pending` before the request
   and `in_progress` once allowed.
5. **Only `selected` with the allow option is approval.** `reject_once`,
   `cancelled`, unknown outcomes, unknown option ids and JSON-RPC errors all
   mean the call does not run. It is reported `failed` and the model gets a
   tool result saying the user declined it (or that permission could not be
   obtained), so every tool call still has a result in the history.
6. **State.** Tools run one at a time, so a pending request blocks all
   foreground work: report `requires_action` while waiting, then `running`
   when the turn continues, whatever the answer. A turn ends with one idle
   update as before.
7. **Cancellation.** When the turn is cancelled, acplause stops waiting at
   once, without waiting for the client's `cancelled` answer and without
   sending `$/cancel_request`; the client must answer pending requests
   itself. The turn ends `idle(cancelled)`. Late answers are ignored.
8. **Resume while waiting.** Over stdio the request stays outstanding on the
   same connection, so resume sends the `requires_action` snapshot and does
   not reissue the request. Connection loss ends the process.
9. **Agent-to-client requests in `internal/jsonrpc`.** `Conn` gains a call
   method used only from session goroutines, never from handlers on the read
   loop. Responses are routed by id on the read loop; responses for unknown
   or abandoned ids are logged and dropped.

## Consequences

- Clients without auto-approval prompt for every change and command.
- Rejection is reported as `failed`; the spec defines no status for it.
- A headless client that never answers blocks the turn until it is
  cancelled.
