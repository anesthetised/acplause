# ADR 0036: Validate MCP transports before session setup

- Status: accepted
- Date: 2026-11-26
- Issue: [#227](https://github.com/anesthetised/acplause/issues/227)
- Supersedes unknown-transport handling in decision 3 of [ADR 0007](0007-mcp-servers.md).
- Protocol: [ACP beta.4 MCP server setup](https://github.com/agentclientprotocol/agent-client-protocol/blob/840484ff59b3deb1314d1a74e351cd16ef052b30/docs/protocol/v2/session-setup.mdx#mcp-servers).

## Context

Clients supply the desired MCP peers in `session/new` and `session/resume`.
Silently skipping a transport could leave a successful session missing a
tool the client expected. Beta.4 requires an invalid-params error for an
unsupported transport or duplicate server name. It gives transport-owned
HTTP headers precedence over configured values.

## Decision

1. Validate the full list before opening peers or changing session state.
   Return `-32602` for a transport other than `stdio` or `http`, or for a
   duplicate server name. Decode opaque variants so validation can reject
   them without interpreting their transport-specific fields.
2. Apply transport-owned HTTP headers case-insensitively. Those headers are
   set or omitted by the transport for each request. Forward other validated
   headers, including client-provided authentication headers, under the
   existing in-memory credential policy.

## Consequences

Clients that relied on silent skipping must remove unsupported peers and
retry. The error arrives before any supported peer in the same list connects.
Connection failures after validation follow the partial setup policy in
[ADR 0038](0038-partial-mcp-setup.md). Package and stdio tests exercise both
setup methods and HTTP header precedence.
[Stdio regression](../../e2e/mcp_setup_validation_test.go)
