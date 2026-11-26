# ADR 0040: Publish client-initiated configuration changes

- Status: accepted
- Date: 2026-11-27
- Issues: [#144](https://github.com/anesthetised/acplause/issues/144), [#227](https://github.com/anesthetised/acplause/issues/227)
- Supersedes: [ADR 0001](0001-scope.md) decision 8 and the notification policy in [ADR 0013](0013-plan-build-mode.md) decision 1
- Protocol: [ACP v2 beta.4 configuration updates][updates] and [replay completion][replay]

## Context

ACP beta.4 recommends notifying active clients of configuration changes,
including changes they requested. Each notification or setter response
replaces the entire configuration list. Out-of-order snapshots could
restore an earlier model or its reasoning options in the client.

## Decisions

1. A successful `session/set_config_option` that changes the advertised
   configuration sends one `config_option_update` before the response.
   Both carry the complete snapshot captured with the mutation, including
   dependent options. Same-value setters return the snapshot and retain
   the model and effort preference behavior of [ADR 0005](0005-remember-model-selection.md).
   Same-value, invalid and pre-commit cancelled requests emit no update.
2. Use the session operation chain for setter admission and output. The
   setter captures configuration under `Session.mu` and waits for writer
   capacity on its worker after unlocking. A committed change still owes
   its notification and response if cancellation arrives during that wait.
3. A setter received during an earlier resume runs after that resume.
   A later replay cursor receives retryable `1001` while a setter owes
   output, following [ADR 0035](0035-resume-write-and-replay-admission.md).
   Snapshot notifications are live, and resume supplies current options
   in its response.
4. Keep the existing mode guards and cold activation rules. A different
   mode is refused when earlier setup or prompt admission is pending
   at receipt for an active session, or foreground work is busy at mutation. Requests staged
   during a cold response write wait for successful activation.

## Consequences

Clients see one coherent replacement even when a model switch also adds or
removes the reasoning effort option. Pipelined prompts and later setters
wait for preceding setter output to enter the FIFO. Catalog refresh for
active sessions and new option types require separate decisions.

[updates]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-config-options.mdx#from-the-agent
[replay]: https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#replay-complete
