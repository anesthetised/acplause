# Setup diagnostics

[Documentation](README.md) · [Project overview](../README.md)

```sh
acplause doctor
acplause doctor --online
```

`doctor` checks configuration syntax, the HTTP(S) API root structure, redacted
API-key presence and data-directory writability. A missing key is informational
for compatible local providers. The API root must have a host and contain no
userinfo, query or fragment. All diagnostics go to stderr; stdout stays empty.
Exit status is `0` when checks pass and `1` on failure. Reports identify failing
checks/settings without printing URLs, keys, paths or raw error values.

The local probe creates the effective data directory if needed (and reports
that creation), writes one temporary file and removes it, including after write
failure. Existing permissions and session databases/leases stay untouched.
Without `--online`, doctor runs locally with no network requests. Neither mode
starts an ACP client/session, MCP server or model inference.
An invocation without arguments remains the ACP stdio server; unknown arguments
fail with usage text. This local check does not establish provider inference or
T3 client compatibility.

`--online` adds an explicit catalog check against the configured provider after
local checks pass. It uses the existing OpenAI-compatible `/models` client and
its retry policy, without following redirects, with one 15-second deadline covering requests,
retries and response decoding. Missing API keys do not block unauthenticated
compatible endpoints. The report gives only a count of distinct selectable
model IDs with tool support: absent parameter metadata is assumed supported,
while an explicit empty list is not. No model is selected or substituted.

Authentication, rate limits, timeout/connectivity, malformed catalogs and no
selectable models fail with safe categories and exit status `1`; successful
local and catalog checks return `0`. Counts establish catalog access and
advertised tool support, not successful inference or T3 interoperability.
Provider messages, model names/descriptions and credentials stay out of reports.

If session setup says no usable model is selected, find a model ID with tool
support in your provider's `/models` catalog, set `ACPLAUSE_DEFAULT_MODEL` in
the ACP client's launch environment and restart the agent. A successful online
diagnostic confirms that the catalog offers selectable models. Check the exact
configured ID against that catalog. See [default model setup](configuration.md#default-model).
