# First-class ACP workflows in T3: runtime acceptance

Status: **pending actual T3 execution**. Automated fixtures prove ACP behavior,
not T3 rendering. The full runtime gate remains [#38](https://github.com/anesthetised/acplause/issues/38),
with feature ordering in [the integration plan](../../tasks/t3/integration.md).

## Structured questions (#30)

Build acplause from the commit being tested and configure it as T3's ACP
provider. Use a temporary workspace, a model with tool support and an isolated
data directory. Record the agent commit, T3 version/build, model ID, date and
transport. Record only sanitized capabilities, method names and state/status
transitions; exclude private answers, prompts, credentials and headers.

1. Confirm T3's initialize request advertises `capabilities.elicitation.form: {}`.
   Ask the model to use `ask_user` with three questions: one single-choice field
   and two free-text fields. Confirm one native question card appears with all
   three required fields and the expected labels and options.
2. Submit complete answers. Confirm `elicitation/create` has `mode: form`,
   flattened `sessionId`/`toolCallId` and `requestedSchema` with required string
   properties. Observe pending tool → `requires_action` → `running` → completed
   tool → idle. Confirm the model acknowledges the answers.
3. Repeat and decline/dismiss the card. Confirm an explicit failed tool result
   reaches the model; dismissing a card does not itself cancel the whole turn.
4. Cancel the turn while the card is open. Confirm best-effort `$/cancel_request`
   references the original request ID, the tool becomes cancelled, and the turn
   ends idle(cancelled). A late answer must not restart model work. Repeat with
   session close/delete where the client exposes those actions.
5. Resume the same live ACP session while waiting. Confirm the pending tool and
   `requires_action` replay before the resume response, with no second question
   request. Answer the original card and confirm only one result.
6. Restart the ACP process while a question is pending, then resume the session
   after ownership can be reclaimed. Confirm the interrupted tool is failed,
   the session is idle, and the old form is not reissued. A later prompt must
   have a matching interrupted tool result in history. Repeat after a graceful
   shutdown: the cancelled tool result must survive restart.
7. Ask for an edit or command after answering a form. Confirm its normal
   permission flow still applies; form acceptance is never permission.

For each step record pass/fail/blocked, actual observations and reproducible
steps. A schema-valid form that T3 cannot display is a client compatibility
failure to report, not a reason to change pinned ACP wire shapes. Do not mark
#38 complete from automated tests or native tools in the parent conversation.

## Structured plans (#32)

Use the same isolated ACP provider setup and record the tested agent commit,
T3 build, model, date and transport. This procedure is pending actual execution.

1. Ask for a small multi-step task using `update_plan` with one `in_progress`
   entry and one `pending` entry. Confirm a native task plan appears and its
   labels, priorities and statuses match the `plan_update` items snapshot.
2. Ask the model to update the plan in the same turn, including a shorter list
   of entries. Confirm the full list is replaced under the same plan ID, with
   no duplicate or leftover entries. An identical update remains a valid event.
   Confirm the tool returns that plan ID without a permission card or workspace
   side effects.
3. Submit another prompt that publishes a plan. Confirm it gets a distinct ID
   and the earlier plan remains in conversation history. Resume the session
   and confirm each plan replays its latest snapshot at its first position.
4. Cancel a turn with unfinished entries, then repeat with a failed model turn.
   Confirm exactly one idle state ends each turn and unfinished entries retain
   their status; neither lifecycle event completes a plan.
5. Restart the ACP provider, resume and confirm both completed and unfinished
   snapshots survive. A new prompt must create a fresh plan ID. Do not replay
   tool execution while restoring historical plans.
6. Exercise malformed arguments or an invalid replacement (for example, two
   `in_progress` entries). Confirm a failed tool result reaches the model,
   no plan update is emitted and the existing snapshot remains unchanged.

Record pass/fail/blocked and sanitized event shapes for each step. Automated
plan events in this repository do not establish T3 rendering acceptance.

## Automated counterpart

```sh
go test -race ./internal/acp ./internal/agent ./internal/jsonrpc
just e2e -run 'Elicitation|Plan'
just check
just test -coverprofile=cover.out
go tool cover -func=cover.out | tail -1
```

Fixtures additionally cover missing capabilities, invalid arguments and
answers, lease loss and cancel/answer races. The e2e validator checks all
outgoing requests/notifications against the pinned root and method definitions,
including protocol cancellation and plan snapshots. Plan fixtures cover full
replacement (including identical updates), per-turn IDs, malformed arguments,
cancelled/failed turns and restart/replay without optional client capabilities.
Other feature runtime procedures remain in
the integration plan until their implementation slices are complete.
