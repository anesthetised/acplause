# ADR 0023: Leave sign-in and credential management outside the agent

- Status: accepted
- Date: 2026-10-21
- Clarifies: the existing auth exclusion in AGENTS.md and the T3 roadmap
- Builds on: [ADR 0007](0007-mcp-servers.md) and [ADR 0022](0022-provider-key-exposure.md)

## Context

acplause is a local ACP agent launched by a client or another process. Its
provider interface is an OpenAI-compatible Chat Completions API. The launcher
already supplies the provider endpoint and key through environment variables;
the ACP client supplies MCP server credentials through explicit configuration.

"Auth is out of scope" can be mistaken for an inability to use authenticated
APIs or an unfinished login feature. The intended boundary is that acplause
uses externally supplied credentials while their acquisition, storage and
renewal belong to the client, launcher or operator. This records that product
decision without changing runtime behavior or the pinned ACP schema.

## Decisions

1. **Keep the provider interface explicit.** Read `ACPLAUSE_API_KEY` at
   process startup and send it as a bearer credential to `ACPLAUSE_BASE_URL`,
   subject to ADR 0022. Without a key, send no `Authorization` header. There
   is no provider-specific fallback variable or automatic credential discovery.

2. **Leave the credential lifecycle outside acplause.** Do not implement
   ACP authentication methods, login/logout commands, browser/device-code or
   OAuth flows, token refresh, credential persistence or OS keychain access.
   The client, launcher or operator obtains and manages credentials and
   supplies them when starting the process. Replacing the provider key requires
   restarting with the new environment. Session resume never supplies a key
   from the session database.

3. **Keep MCP credentials separate.** Use the client-provided `env`/`headers`
   under ADR 0007. The client supplies replacement configuration on resume.
   Keep that configuration in memory and do not add agent-owned MCP OAuth,
   interactive sign-in or token refresh. The provider key is not implicitly
   forwarded to MCP servers.

4. **Retain independent protections.** External credential management does
   not relax key redaction, redirect rejection, subprocess environment handling
   or the existing ban on asking for secrets through model questions. Tool
   permissions remain governed by ADR 0003 and subsequent tool-specific ADRs;
   possession of an API key never approves a file change or command.

## Rationale

The launch environment works with the current stdio architecture and API-key
providers, including endpoints that require no key. It lets different clients
choose their own secret storage and user interface while reusing the same
agent binary.

Provider account flows differ from the shared Chat Completions request format.
Embedding them would add provider-specific browser callbacks, refresh state
and recovery behavior to a provider-independent agent. A second credential
store would also introduce synchronization and deletion rules alongside the
client's existing configuration. The current scope does not need those costs.

Alternatives considered are agent-owned interactive sign-in and a built-in
credential/keychain store. Both can improve standalone onboarding, but that
benefit does not justify taking ownership of accounts and credential lifecycle
in this client-launched agent. They remain deliberately excluded.

## Consequences

Users configure credentials before use. Expired or rejected credentials must
be replaced outside acplause; the agent cannot repair them by opening a login
flow. Authentication failures still surface through the existing provider/MCP
error paths, and `doctor --online` can check provider catalog access without
performing sign-in.

Environment configuration is a delivery mechanism, not a secret vault or OS
sandbox. ADR 0022's same-user access limitations still apply. Credentials
supplied as configuration are not saved as session settings; secrets pasted
into prompts or emitted by tools are conversation content and can be persisted
under the normal storage policy.

Future work on skills, background processes, delegation or sandboxing must
preserve this boundary. Reconsidering it requires a new ADR with a concrete
need and explicit ownership model, rather than treating missing login as a
feature defect.
