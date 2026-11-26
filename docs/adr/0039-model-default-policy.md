# ADR 0039: Require a usable model at session setup

- Status: accepted
- Date: 2026-11-27
- Issue: [#227](https://github.com/anesthetised/acplause/issues/227)
- Protocol: [ACP v2 beta.4 config defaults](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-config-options.mdx#default-values-and-graceful-degradation)

## Context

ACP requires every advertised `currentValue` to be usable even when the
client skips model configuration. The empty model choice from
[ADR 0002](0002-client-model-selection.md) required a separate config request
before the first prompt. Restoring a session whose model left the catalog
could also leave it without a usable model. Selecting an arbitrary catalog
entry could send prompts to a model the user or operator did not authorize.

## Decision

1. Resolve the new session's model from the available tool-capable catalog.
   Prefer the last explicit model choice in the data directory under
   [ADR 0005](0005-remember-model-selection.md). During cold resume, prefer
   the saved session's own model. Either choice retains its effort when the
   model supports it and the value is valid. Otherwise use provider-default
   effort for that model.
2. Use `ACPLAUSE_DEFAULT_MODEL` as the launcher-authorized fallback. It must
   match a usable catalog ID and is used at provider-default effort. This also
   covers legacy sessions with a blank saved model. A warm resume keeps its
   live settings and catalog.
3. Advertise the effective model as a value in the full nonempty selector,
   keeping the flat or grouped layout from [ADR 0018](0018-group-model-options.md).
   If neither choice qualifies, return `-32603` with launcher setup guidance
   before session creation or MCP startup. On a failed cold resume, release
   ownership and preserve the stored session and history without replay or a
   restoration save. Catalog load failures keep their existing error mapping
   and cache behavior.
4. Persist the effective model with the session. A successful cold resume
   saves an authorized replacement in that session. The global preference
   still changes only through explicit model or effort requests from the
   client.

## Consequences

- A fresh data directory needs a valid launcher default before the first new
  session. Once a user has an available remembered choice, new sessions can
  reuse it with the launcher default unset.
- The first prompt can use the effective model directly, including on clients
  that omit model and reasoning controls.
- Removed models require an authorized replacement before cold resume can
  proceed. A successful replacement becomes the session's saved selection.
