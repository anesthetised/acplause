# ADR 0038: Continue session setup with available MCP servers

- Status: accepted
- Date: 2026-11-27
- Issue: [#227](https://github.com/anesthetised/acplause/issues/227)
- Partly supersedes rollback for individual peer failure in decision 5 of [ADR 0007](0007-mcp-servers.md) and the connection-failure consequence of [ADR 0036](0036-validate-mcp-transport-at-setup.md).
- Protocol: [ACP beta.4 MCP server setup](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#mcp-servers) and [session notices](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/prompt-lifecycle.mdx#session-notices).

## Context

ACP beta.4 recommends proceeding when a configured peer cannot launch or
connect and telling the user. Rolling back for an individual peer failure
also withholds tools from healthy peers and prevents work with built-in tools
when every requested peer is unavailable.

## Decision

1. Validate the entire request before connecting. During `session/new` and
   `session/resume`, try all valid entries within the shared 30-second setup
   budget, with at most four connections starting together and up to 15 seconds
   for discovery per peer. Publish the successfully initialized and discovered
   peers together with any unchanged healthy connections, including a peer with
   matching configuration that recovered while setup was in progress. An empty
   successful subset still permits session setup and use of built-in tools.
2. A committed resume replaces the previous configuration with that subset.
   Discard a failed replacement and revoke and close its predecessor, including
   a predecessor with the same server name. Revocation cancels pending permission
   by its original JSON-RPC request ID. A late allow cannot authorize dispatch.
   Cancellation, loss of session ownership or setup invalidation before commit
   rolls back the change and closes newly created candidates. Warm resume checks
   the durable lease at publication. Cursor resume fences publication against
   overflow and cancellation at replay completion.
3. After a successful response, send one live `warning` notice if any entries
   failed. Use `MCP setup incomplete` when a peer is available and
   `MCP servers unavailable` when all requested peers failed. Identify omissions
   by one-based input position, supported transport and a fixed reason. Keep
   configuration values, endpoints, upstream IDs and raw errors out of the
   notice. On a cursor resume the history, `replay_complete` and current state
   precede the response. Setup owes the warning until it is queued. A conflicting
   cursor can retry after that point, and close replies after setup output.
   Setup notices are excluded from storage and replay.

## Consequences

Users can continue while an MCP peer is unavailable. To retry a failed peer,
the client can send the desired configuration in a later resume. Full
replacement prevents a failed setup from retaining an older endpoint or
credentials. The notice gives users an entry to locate in their configuration
without exposing its contents.
