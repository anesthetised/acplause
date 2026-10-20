# ADR 0022: Keep the provider key on the configured API root

- Status: accepted
- Date: 2026-10-21
- Issues: [#107](https://github.com/anesthetised/acplause/issues/107)
- Extends: [ADR 0007](0007-mcp-servers.md), which already strips provider credentials from MCP stdio servers and rejects MCP redirects

## Context

The provider key could leave the configured endpoint in three ways. A key
meant for OpenRouter, `OPENROUTER_API_KEY`, was used for any
`ACPLAUSE_BASE_URL`. The model client used `http.DefaultClient`, and Go keeps
`Authorization` on redirects to the same host or a subdomain, even on another
port. The bash tool inherited the whole environment of acplause, so a
permitted `env` or `printenv` put the key into model context, the ACP terminal
and the SQLite transcript. MCP stdio servers already get a minimal environment
without provider credentials (ADR 0007 #4).

## Decisions

1. **One variable.** The key comes only from `ACPLAUSE_API_KEY`, whatever the
   base URL. `OPENROUTER_API_KEY` is not read. Without a key, requests carry no
   `Authorization`.

2. **No redirects.** The model client follows no redirect. A 3xx response
   fails with a non-retryable error that asks for the final API root. This
   matches the MCP policy in ADR 0007.

3. **Unset after reading.** `serve` reads its configuration, then removes
   `ACPLAUSE_API_KEY` from its own environment, so bash commands and anything
   else acplause starts do not inherit it. The rest of the environment is
   inherited unchanged: commands need `PATH`, SSH agents, proxies and toolchain
   variables, and a minimal environment like MCP's would break them.

4. **No values in logs or errors.** `serve` logs the base URL without userinfo,
   query or fragment. Configuration errors name the variable and the expected
   form, never the value.

## Consequences

Users of `OPENROUTER_API_KEY` must set `ACPLAUSE_API_KEY`. A provider that
redirects needs its final URL configured.

Decision 3 prevents accidental exposure, not a deliberate read. bash runs as the
same user as acplause and can still read the initial environment of its parent
where the platform exposes it (`/proc/<pid>/environ` on Linux), shell profiles
and files that hold the key. Real isolation would need another user or a
sandbox, which acplause does not provide; every bash call already needs
permission (ADR 0003).
