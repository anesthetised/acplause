# MCP servers

[Documentation](README.md) · [Project overview](../README.md)

The ACP client supplies `mcpServers` in `session/new` and `session/resume`.
Both transports use the same discovery, permissions and tool history:

```json
{
  "mcpServers": [
    {
      "type": "stdio",
      "name": "workspace",
      "command": "/absolute/path/to/mcp-server",
      "args": ["--stdio"],
      "env": [{"name": "SERVER_TOKEN", "value": "client-supplied-token"}]
    },
    {
      "type": "http",
      "name": "remote",
      "url": "https://mcp.example.com/mcp",
      "headers": [{"name": "Authorization", "value": "Bearer client-supplied-token"}]
    }
  ]
}
```

## Credentials

The client supplies any server credentials through the stdio `env` or HTTP
`headers` configuration shown above. acplause uses those supplied values and
keeps the configuration in memory; it does not obtain or refresh credentials
through OAuth or interactive sign-in. The client supplies updated configuration
on resume. This follows the same
[external credential-management boundary](adr/0023-external-credential-management.md)
as provider setup; `ACPLAUSE_API_KEY` is not an implicit MCP credential.

## Transports and discovery

Stdio executes the absolute command directly, without a shell, in the
session's `cwd`. It inherits only `HOME`, `PATH`, `USER`, `LOGNAME`,
`TMPDIR`/`TMP`/`TEMP`, `LANG` and `LC_*`, plus Windows system/profile
variables; explicit `env` entries override these. Provider API keys are not
inherited implicitly. MCP servers run with the agent's OS privileges,
outside the built-in file sandbox. Server stderr is discarded.

On Windows use an absolute executable path, such as
`C:\\tools\\mcp-server.exe` in JSON (backslashes must be escaped). To launch a
`.cmd` entry point, configure an explicit interpreter such as
`C:\\Windows\\System32\\cmd.exe` with `/d`, `/c` and the command in `args`;
acplause does not automatically run MCP commands through PowerShell. Environment
variable overrides are case-insensitive on Windows.

acplause requests MCP `2025-11-25` and also accepts `2025-06-18`. It reads
every tool-list page, retains input schemas, assigns stable aliases to avoid
name collisions, and refreshes changed catalogs. Every external call needs
explicit ACP permission, including tools marked read-only. Nested arguments,
structured-only output and opaque IDs remain JSON; the current model API
receives structured output as text, and ACP `rawOutput` retains its structure.

HTTP supports JSON and SSE responses, optional GET event streams (405 is
allowed), session/version headers and best-effort DELETE on close. Interrupted
SSE results resume only by GET with `Last-Event-ID`; a missing cursor fails
the call. Redirects are rejected. Legacy HTTP+SSE, OAuth, interactive auth,
roots, sampling, elicitation and MCP Tasks are not supported.

## Reconfiguration and call bindings

Catalog refresh revokes changed or removed definitions while a call awaits
permission or transport capacity. Dispatch is admitted atomically with refresh,
after an HTTP slot is acquired or the stdio writer selects the call, immediately
before I/O. An admitted call may finish on its original connection even if its
definition changes or disappears. Turn cancellation, timeout, connection
replacement, session close/delete and ownership loss still stop it. Admission
does not imply that the remote server received or completed the operation.

Resume replaces the entire MCP configuration; an omitted or empty list
removes all servers. Healthy unchanged connections are reused. Failed setup
preserves the previous configuration; later prompts wait for setup before
insertion. Changed/removed connections invalidate earlier model tool
bindings, so an old approval cannot invoke a replacement endpoint. Configs,
environment values, headers and upstream session IDs are kept in memory.
Tool results follow the normal conversation storage policy.

## Timeouts, cancellation and cleanup

Setup allows 30 seconds, with at most four connections starting together;
discovery/refresh allows 15 seconds per server. `ACPLAUSE_MCP_TIMEOUT` starts
after permission and covers dispatch waiting and the response, independently
of the bash timeout. Cancellation stops local waiting promptly and sends a
best-effort MCP cancellation notification. A
remote side effect may still complete. Calls are never automatically repeated
after an ambiguous failure. Crashed stdio servers restart on explicit resume;
an expired HTTP session gets one recovery attempt for future model requests.

Close/delete/shutdown/ownership loss release connections. Stdio cleanup closes
stdin, waits one second, sends TERM, waits one second, then kills and reaps;
Unix cleanup includes the process group. Windows creates the server suspended
atomically inside a private kill-on-close Job Object before resuming; after
the stdin grace period, cleanup terminates the job and its members. Agent death
also closes the job handle. This is process cleanup, not an OS sandbox; external
services/brokers can start work outside the job. A failed stdio process cleanup
emits one warning to stderr, whether closure was explicit or automatic; it
omits raw errors and configuration to avoid exposing credentials.
HTTP DELETE has a two-second deadline. Limits are 16 servers,
100 pages/1,024 tools per server, 8 MiB per message/event/catalog and 256 KiB
per projected text/JSON result; exceeding a limit fails explicitly.

## Tool results and images

Text, embedded text resources, resource links and validated PNG/JPEG tool
images are supported. Images reach the ACP client and the vision model, with
their originating tool-call IDs. Chat Completions receives all sibling tool
results before a separate user message labeled as untrusted tool-image context.
The model selected for that round must explicitly advertise image input;
text-only or unknown models get omission markers, without a model switch.

Tool images have the same per-image and per-result limits as prompt images:
4 MiB and 16 million pixels per image, four images and 8 MiB decoded total per
result. The 8 MiB MCP frame limit still applies before image validation.
Invalid, unsupported and excess images produce omission markers, retaining
usable text/JSON and images that fit. Audio and blob results remain omitted.
An unsupported-only result fails with a possible-completion warning; image
projection never repeats the remote operation. Images persist for resume and
replay, while summaries and ACP rawOutput carry descriptors without base64.
Actual T3 browser screenshots reached the vision model and fresh visual markers
were recognized. In the tested T3 build, the expanded ACP tool card displays
only the image descriptor, without an inline image; this client display gap is
tracked in [#71](https://github.com/anesthetised/acplause/issues/71), separately
from successful model delivery. See the
[runtime smoke record](testing/t3-first-class-smoke.md).
See [ADR 0009](adr/0009-image-tool-results.md),
[ADR 0007](adr/0007-mcp-servers.md) and the
[real T3 smoke record](testing/mcp-t3-smoke.md).
