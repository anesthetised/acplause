# First-class ACP workflows in T3: runtime acceptance

Status: **partially verified in actual T3 on 2026-10-19; `/compact` failure
reproduced on 2026-10-21; acceptance remains open**.
The `/compact` and tool-image display client gates failed; see
[#70](https://github.com/anesthetised/acplause/issues/70) and
[#71](https://github.com/anesthetised/acplause/issues/71). Keep
[#38](https://github.com/anesthetised/acplause/issues/38) and
[#25](https://github.com/anesthetised/acplause/issues/25) open. Automated fixtures
prove ACP behavior, not T3 rendering. Feature ordering is recorded in
[the integration plan](../../tasks/t3/integration.md).

## `/compact` retest: 2026-10-21

**FAIL: #70 remains reproducible after rebuilding acplause.** Tested at
00:35–00:37 UTC through the actual T3 ACP provider.

- Agent: `7ae7319b87b57b4b04f3b57ec64c0df3d27665f1`, Go 1.27.2,
  darwin/arm64, installed binary SHA-256
  `fb197e95200f6dafe4f5cbadacfaad295594e66ed67069c0e51801f2a50ba323`.
- Client: T3 Code (Beta) 0.1.0; `app.asar` SHA-256
  `ed8c4ef3e1c8b8531d05dbf3d12a27f5e4a715105e2eaaac2e910f739a21128d`.
  This is the same client artifact as the original failing run.
- Provider: `acpRegistry_openrouter`, `openai/gpt-6.1-sol`, High · Build,
  ACP stdio. Reused the existing `ACP #38 actual-client smoke` T3 thread.
  Its old session load failed; T3 created ACP session
  `5a6134fd-0bf6-4eee-8e4f-be8750292e4f` and supplied a context handoff.
  This retest does not establish successful ACP resume.

An ordinary baseline prompt returned `COMPACT_BASELINE_OK` (T3 run 22).
In the native composer, entering `/` offered only `/model`; entering
`/compact` displayed `No matching command.` Submitting the exact command
without attachments failed at 00:36:29 UTC (run 23). A separate
`t3_thread_send` of the exact command failed at 00:36:41 UTC (run 24).
Both displayed `The provider could not start this turn. Retry the turn; if
it keeps failing, check the provider setup and server logs.` An ordinary
follow-up returned `COMPACT_RECOVERY_OK` (run 25).

T3's native request log recorded successful `session/prompt` requests for
the baseline and recovery, with no request recorded for either command
attempt. A read-only check of the new ACP session after both failures found
it idle, with only the baseline user/assistant pair in history and two
message snapshots in the transcript: no inserted command or compaction.
Together these observations reproduce the client dispatch blocker. This
run used native request metadata and stored-session metadata, without the
original run's transparent wire proxy; it did not independently capture
`availableCommands` or wire-level state updates.

The existing provider used its normal data directory; only the smoke
session was inspected, and no provider configuration, binary, or database
was manually changed. No repository changes were requested from the ACP
agent. `just check` passed, including race-enabled tests and schema-validating
e2e tests. Keep #70, #38 and #25 open; #71 was not retested.

## Observed runtime: 2026-10-19

Execution began at 21:36 UTC.
This was the built acplause binary running as a **child ACP provider inside T3**.
The parent's Codex tools were used to prepare fixtures, operate the client and
inspect sanitized evidence, never as substitutes for the child's tools.

- Agent: `8f2064dffb41d99f32a7f0cefba8bea9e73c2715`, Go 1.27.2,
  darwin/arm64. Binary SHA-256:
  `baae88a76981a1510fdb7ae3a693ac12086ebc52266dcdcf603ca8a6737c4c55`.
- Client: T3 Code (Beta), version **0.1.0**, bundle build **0.1.0**.
  `Contents/Resources/app.asar` SHA-256:
  `ed8c4ef3e1c8b8531d05dbf3d12a27f5e4a715105e2eaaac2e910f739a21128d`.
- Provider: `acpRegistry_openrouter`; vision model `openai/gpt-6.1-sol`,
  high reasoning effort; text-only rejection model `deepseek/deepseek-v3.2`.
  OpenRouter's live model metadata explicitly listed `image` for the former
  and only `text` for the latter. No inference from model names was used.
- ACP: stdio, pinned `2.0.0-alpha.9` at
  `3be9cfc8ea2dfe5a28973b461fd956b4045f24d3`. Injected MCP: stdio, server
  name `t3-code`. This run makes no real-client HTTP claim.
- T3 delegation request: `acplause-38-actual-20261020-1`; ACP session:
  `4471c630-ca28-47c3-91ad-e0b7dfb186b0`.
- The child used the repository working directory, with no repository writes.
  Generated image fixtures and the isolated `ACPLAUSE_DATA_DIR` were under
  `/tmp/acplause-smoke-38-20261020`; no production session database was used.
  Prefer a disposable workspace when repeating the procedure.

The following implementation commits were verified as ancestors of the tested
agent, rather than relying only on closed issue status:

| Dependency | Implementation present |
| --- | --- |
| #23 MCP tool images | `9fc33aa` |
| #28 prompt images | `b552963` |
| #30 forms | `c8ff68a`, `ac351e2` |
| #32 plans | `473eeef` |
| #34 Plan/Build | `5a306fa` |
| #36 terminals | `4826384` |
| #37 `/compact` | `0a801a8` |

A temporary external proxy forwarded ACP lines unchanged and recorded only
methods/IDs, block types and lengths, model/mode selections, capability shapes,
MCP environment/header **names**, and tool/plan/terminal/state metadata. It
did not record prompt bodies, answers, image bytes, endpoint values, credentials
or authorization values. Native UI observations and the isolated database
were checked separately. Raw client logs and full-window screenshots are not
included here because they can contain unrelated user data.
After execution the temporary proxy was removed, the installed binary's
original checksum was verified, and the isolated provider and fixture server
were stopped. No provider settings or production session data were changed.

### Results

| Workflow | Result | Observed evidence |
| --- | --- | --- |
| Clipboard PNG | PASS | Preview → Select All/Copy → actual Cmd-V in T3 produced `image.png`; ACP carried `image/png`; the model read `233163`, red. |
| File-upload JPEG | PASS | T3 Attach files/Open selected `upload.jpg`; the model read `228742`, blue. |
| Image-only composer input | PASS | JPEG sent with no user-entered text; T3 supplied its own attachment-only placeholder text and the image reached the model. This was not a pure image-only ACP payload. |
| Multiple prompt images | PASS | Two visible attachments became two ACP image blocks in order; the model read `233163`/red, then `887979`/green. |
| Text-only model rejection | PASS | T3 displayed `selected model "deepseek/deepseek-v3.2" is not known to accept images`; ACP returned `-32602` without a `messageId` or inserted user message. Model selection was not silently changed. |
| Browser screenshot to model | PASS | Child MCP `preview_snapshot(includeImage: true, save: false)` returned a PNG; after reload the model read a fresh `504403`, red, from a canvas with empty visible text. |
| Browser tool image in T3 card | FAIL: client display | Expanded native screenshot tool card showed only the descriptor, without an image/preview control. The separate floating browser preview is not the ACP tool image. Tracked in #71. |
| Form answers | PASS | T3 advertised `elicitation.form: {}`; native question UI collected three required fields; the model acknowledged all three submitted sample answers. |
| Form cancellation | PASS | Stop while a native question was open cancelled the request/tool and ended the ACP turn with idle(`cancelled`); a later turn saw the cancelled tool result. |
| Plan replacement | PASS | Native Tasks changed from 0/3 to 1/2; both calls returned plan ID `6ddcdb8e-a5d7-497e-a241-190375ae344a`, without leftover entries or a permission request. |
| Plan/Build | PASS | T3 showed High · Plan and sent `mode=plan`; bounded `list_dir` and `update_plan` worked. The model's available tools excluded bash/write/edit/MCP. High · Build restored MCP and bash execution. Forced forbidden calls are covered by automated tests, not claimed from this model observation. |
| Streaming command/cancel | PASS | Native command card grew from live chunks to `SMOKE_TICK_023`; Stop produced terminal exit `SIGKILL`, tool `cancelled`, then one idle(`cancelled`). |
| Restart/resume | PASS with retry | After graceful ACP process exit, the first immediate follow-up failed. Retrying started a new process and explicit `session/resume` of the original session, replaying images, two plans and the terminal snapshot before its response. |
| `/compact` discovery/execution | FAIL: client | ACP advertised `compact` on new/resume. T3's slash menu offered `/model` only, `/compact` showed `No matching command.`, and submission failed before any ACP `session/prompt`. Tracked in #70. |
| Async MCP regression after the above | PASS | Child called capabilities/delegation; automatic completion entered a later ACP turn, which called `task_status` and browser status through MCP. Details below. |

### Image evidence and reproduction

The fixtures were generated locally as 660×280 images with large six-digit
bitmap glyphs and a colored rectangle. Each marker was chosen randomly and
was absent from the child prompt, filename, DOM text and tool arguments.
The model was instructed not to read files or inspect DOM/scripts. Record
each of UI attachment rendering, ACP delivery and model recognition separately
when repeating these cases; a successful upload alone proves none of the others.

| Fixture/source | Expected and observed | Original fixture SHA-256 |
| --- | --- | --- |
| Clipboard source PNG | `233163`, red | `fabf72388fd2d601611a999890ca9451c74ad72cd35486eb68f5c7cab9c25cf8` |
| Uploaded JPEG | `228742`, blue | `e59ce744010ef17f006f2bb0d1260e850a3be01a196064c4171255f1a92cbab2` |
| Second uploaded PNG | `887979`, green | `f6e51cf4e2b6ba7d38a2a72c41da1118c4fcf9f7805287317e607e3f2eefc4a9` |
| Fresh browser canvas PNG | `504403`, red | `97be7c1150cb8de0ef9c086ca681ec7f63d4234b95161cec43c57541745dbca3` |

To reproduce, generate new markers and keep their expected values outside the
child's accessible prompt/tool context. Copy the PNG's pixels from Preview,
paste in the T3 composer and ask it to read the marker/color. Independently use
the native file picker for JPEG, submit with an empty composer, then upload
two images and request answers in attachment order. Select a catalog-confirmed
text-only model and submit an image; verify rejection before insertion and no
automatic model substitution, then restore the vision model.

For the tool-image case, serve a local HTML canvas that draws a fresh PNG and
has no textual answer. This run used `http://127.0.0.1:18338/browser.html` and
tab `tab_8_f96728c8-e7f0-4467-9e94-d91ef59541a4`. MCP `preview_open` succeeded;
initial `preview_navigate` calls failed because the model supplied both `url`
and `target`, or explicit null values. Those errors reached the model as
failed tool results. Repeated navigation attempts were cancelled; a follow-up
called `preview_snapshot` with an explicit valid tab ID, `includeImage: true`
and `save: false`.

The first screenshot showed a previously uploaded marker, so it was **not**
counted as independent vision evidence. A fresh fixture was generated; F5
did not refresh it in this browser, so the child used `preview_evaluate` only
for `location.reload()` and took another screenshot. It read the new marker
correctly. The returned 1280×800 PNG was retained as a typed tool-history
image; ACP tool content included the image, while rawOutput contained only
its descriptor. No file-path image or parent screenshot tool substituted for it.

A final display probe repeated the MCP screenshot, then held a native form
open while the tool card was inspected. Expanding both the tool group and
"Took a snapshot of the preview page" showed arguments, URL and the image
descriptor only. Native accessibility state contained no tool-image element,
and visual inspection confirmed no inline image. The live floating browser
preview separately displayed the canvas. This is a client display failure,
not a model-delivery failure; see #71. All 65 final saved transcript snapshots were
also reconstructed in memory as ACP notifications and validated against the
pinned root schema and `UpdateSessionNotification`, including four tool-image
blocks. No payload bytes were printed or exported by that check.

### Plans, terminal and restart evidence

The first plan replaced three entries with two under the same ID, leaving one
completed and one in progress. A later Plan-mode turn created the distinct
ID `4341c1c5-2859-4c13-b677-2dfe16e04cc4` with one in-progress and one pending
entry. Both latest snapshots were replayed after process restart without
completing their unfinished entries.

The child ran this command using its agent-owned bash tool:

```sh
i=0; while [ "$i" -lt 120 ]; do
  i=$((i+1)); printf 'SMOKE_TICK_%03d\n' "$i"; sleep 1
done
```

T3 received live `terminal_output_chunk` events and showed the growing output
in its expanded native command card. Cancellation stopped it after 23 lines.
Terminal `3b9292d4-a040-4de8-87a7-f82cedc624d0` persisted a 345-byte snapshot
ending in `SMOKE_TICK_023` and exit `{signal: "SIGKILL"}`. It replayed as one
`terminal_update`, without reexecuting the command. The same output remained
visible in T3 after resume. The model's cancelled bash result contained only
the cancellation notice, so its inability to quote the final tick from model
history was expected; the terminal snapshot is client transcript state.

After SIGTERM of the idle test provider, the first immediate follow-up failed
before reconnection. Retry observed `initialize` → `session/new` →
`session/resume` with the original ACP session ID and the injected stdio MCP
configuration. User/tool images, both plan snapshots and the terminal snapshot
were sent before the resume response. The resumed model correctly recalled
the image marker/color pairs. A second graceful restart and an ordinary
`RECOVERY_OK` turn also succeeded. This is not crash-during-stream coverage.

### Async MCP regression

After the `/compact` failure, the same ACP child successfully called
`orchestrator_capabilities` and `delegate_task` through its injected MCP tools,
targeting Codex `gpt-6.1-sol` with request ID `acplause-38-nested-20261020`.
The nested task used no tools or file changes and returned
`ACPLAUSE_38_ASYNC_OK`.

```text
taskId: node:delegated-task:command%3Amcp%3A19f00cb4-c8a2-4af0-80a8-045624119343%3Adelegate-task%3Aacplause-38-nested-20261020
```

Delegation returned asynchronously and the ACP child ended its turn. T3's
completion notification then became a later ACP prompt. In that turn the
child called `task_status`, observed `completed` and the exact marker, then
called `preview_status` with a deliberately missing tab ID and received
`available: false`. Neither the parent nor a CLI fallback performed those
regression calls.

### Remaining gates and limits

- `/compact` cannot be accepted in this client build: repeat its menu and
  execution checks after #70 is resolved. Submission was tried through both
  native UI and T3 MCP, including after a successful ordinary recovery turn;
  none dispatched a command prompt to acplause. No command/schema workaround
  was added to the agent.
- Tool images reach the vision model but do not appear inside the expanded
  T3 ACP tool card. Repeat the separate display gate after #71 is resolved;
  do not put base64 in rawOutput or alter pinned wire shapes to work around it.
- The extended form procedures below (separate decline, late-answer race,
  live resume while pending, pending-form restart, close/delete) and malformed
  plan replacement/error cases remain automated coverage, not observations
  from this run. Crash-during-active-stream recovery is also unverified here.
- MCP navigation needed argument guidance; explicit nulls and mutually
  exclusive fields were not rewritten by the agent. This is not a claim that
  arbitrary models autonomously operate every T3 browser tool successfully.
- Checkpoint rollback, native subagent lineage, rewind, active steering and
  auth are outside this ACP v2 roadmap. Working MCP delegation does not imply
  native provider parity.

### Automated verification

On the tested agent, `just check` passed gofmt, vet, Staticcheck, all tests
with the race detector (including schema-validating e2e), and `go mod tidy
-diff`. `just test -coverprofile=…` also passed; `go tool cover -func=…` reported
**89.6% total Go statement coverage** (required minimum: 80%). These checks
do not change the failed actual-client `/compact` and tool-image display gates.

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
T3 build, model, date and transport. The record above identifies the subset
actually executed; the complete procedure below remains the acceptance checklist.

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
Other feature runtime observations and reproduction steps are recorded above;
the integration plan remains the scope and acceptance contract.
