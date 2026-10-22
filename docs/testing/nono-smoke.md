# External nono wrapper: actual T3 smoke

Date: 2026-10-22. Related: [#133](https://github.com/anesthetised/acplause/issues/133),
[ADR 0026](../adr/0026-shell-lifecycle-after-seatbelt.md).

**Result: the tested external launcher works without an acplause integration,
with the client caveats below.** Actual T3 ACP turns exercised the model API,
bash, an injected stdio MCP server, cancellation and SQLite session resume.
This is a compatibility smoke, not validation of nono's complete security
contract or a replacement for ADR 0025's failed lifecycle gate.

## Recorded environment

- macOS 27.0.1, build 26A434, arm64; Go 1.27.3.
- T3 Code (Beta) 0.1.1; existing provider `acpRegistry_openrouter` and existing
  `ACP #38 actual-client smoke` thread. No new delegated work was created.
- Actual OpenRouter model `openai/gpt-6.1-sol`, high reasoning, Build mode.
  No mock model endpoint or fake ACP client stood in for these T3 turns.
- nono 0.80.0, installed by the operator through Homebrew at
  `/opt/homebrew/bin/nono`; SHA-256
  `12f6962089f860e6b6fc9c1440fbd8f1c04cac791154e7abf74df91700976fb7`.
- acplause built with `go build -o <fixture>/bin/acplause ./cmd/acplause`
  from `ae449f28427ed125e6d4cf455da6e3a7a0170160`; SHA-256
  `cbba6fa62105e53776d59553b88970680620dc185d5bc750f7b49f4a76caed6a`.
  Its version includes `+dirty` because the disposable workspace fixture was
  an untracked repository subdirectory; production sources were unchanged.

The wrapper used a profile extending nono's built-in `default`, explicit
read access to the repository and fixture executable directory, and write
access only added for a disposable workspace subdirectory, separate acplause
data directory and scratch. Inherited default groups retain their own grants;
these additions are not a statement that all other host paths are denied.
Networking remained allowed. Proxy filtering, credential injection, capability
expansion and per-command policies were not enabled or tested.

## Launch and evidence

T3's existing executable setting was temporarily changed from `acplause` to a
fixture launcher. Its stored provider key remained in T3's existing secret
storage. The launcher inherited that value normally and never printed it.
It set `ACPLAUSE_DATA_DIR`, `TMPDIR`, `XDG_CONFIG_HOME` and `XDG_STATE_HOME`
to test-owned locations, then ran:

```sh
nono run --silent --no-audit --no-diagnostics --startup-timeout 0 \
  --profile /absolute/path/to/profile.json -- /absolute/path/to/acplause
```

A transparent parent proxy forwarded ACP lines unchanged. It recorded only
directions, methods/IDs, session IDs, update kinds/statuses, terminal/tool IDs,
and MCP configuration names/environment-variable names. It did not record
prompt bodies, tool arguments/results, credentials, environment values or
authorization headers. No stdout/stderr merging or PTY was used.

The actual session was `283b8d49-2c3d-42b5-a2b2-5a96e22f1e34`. T3 supplied its
`t3-code` MCP server over stdio with `ELECTRON_RUN_AS_NODE`,
`T3_ACP_MCP_ENDPOINT` and `T3_ACP_MCP_AUTHORIZATION`. Parent MCP calls prepared
and inspected the test; the MCP result below came from the wrapped child.

## Observations

| Check | Result | Evidence |
| --- | --- | --- |
| ACP stdio and actual model API | PASS | T3 completed run 26 with `NONO_ACP_BASELINE_OK`; protocol initialization, configuration, prompt and idle updates crossed the wrapper. No non-JSON stdout lines were observed. |
| Bash and workspace writes | PASS | Run 27 used the built-in bash tool to write and read `NONO_BASH_OK` in the disposable repository subdirectory. Permission and terminal/tool updates crossed ACP. |
| Outside fixture restrictions | PASS for these paths | The same bash invocation reported `OUTSIDE_READ_DENIED` and `OUTSIDE_WRITE_DENIED` for synthetic files under an ungranted home subdirectory. Unconfined controls read/wrote those files successfully; the confined write target remained unchanged. |
| Injected stdio MCP | PASS | Run 27 called `orchestrator_capabilities` through acplause's MCP relay and completed successfully. No parent-tool or CLI substitute was used. |
| Cancellation | PASS for attached command | Run 28 started an attached `sleep 120` shell fixture. T3 interruption sent `session/cancel`; the terminal finalized, the tool became `cancelled`, and exactly one final `idle/cancelled` update followed. The shell exited and its post-sleep marker was absent. |
| Recovery after cancellation | PASS | Run 29 returned `NONO_CANCEL_RECOVERY_OK`; cancelled tool results were retained in history. |
| Stored session and process restart | PASS after retry | The idle fixture acplause process was identified by its exact executable path and stopped gracefully with SIGTERM. Run 30 failed in T3. Run 31 reconnected, sent `session/resume` for the same session ID and returned `NONO_RESUME_OK`. Read-only SQLite inspection confirmed the original history and new messages in the separate data directory. |

The stored original session remained idle, with 15 history rows after the
successful resume. T3 also created an empty session during reconnection; it did
not replace the resumed conversation. Both belonged to the disposable database.
This run inspected real T3 turn records and ACP metadata; it does not make a new
claim about visual terminal rendering, forms, images or `/compact`.

## Client and configuration caveats

- **Cold HOME-based discovery is not established.** T3's background discovery
  issued `session/new` with `cwd=/Users/vladimir` and no MCP servers. The profile
  did not grant that workspace, and acplause returned `-32602`. A separate
  piped control reproduced `cannot open workspace: ... operation not permitted`.
  Real thread turns used the explicitly granted repository and succeeded.
  This test reused an existing provider/model catalog; it does not establish
  fresh T3 model discovery with this narrow profile. Granting recursive home
  access would change the intended scope and was not used as a workaround.
- **The first turn after process shutdown failed.** Run 30 ended with T3's
  `Provider turn failed.` and no new thread prompt/resume in the captured ACP
  stream. One explicit retry succeeded. The evidence does not establish the
  client's exact failure cause or seamless automatic recovery.
- **Use an explicit launcher working directory.** Running from an ungranted
  initial directory produced nono's noninteractive CWD approval error. The
  final launcher selected the granted repository before invoking nono.
- **Keep nono state outside broadly readable runtime roots.** Placing its
  `XDG_STATE_HOME` under `/private/tmp` failed because `system_read_macos`
  would expose protected nono state. Separate test-owned state/config
  directories under home worked without granting recursive home reads.
- **Validate against the installed version.** nono 0.80.0 rejected the
  `interactive` profile field shown in a website example. Removing that field
  produced a valid profile; the actual `nono profile validate` command passed.

## Reproduction scope

Use a trusted local profile, an explicitly selected workspace, an isolated
`ACPLAUSE_DATA_DIR` and synthetic outside files. A minimal profile shape for
the tested compatibility pattern is:

```json
{
  "meta": {"name": "acplause-smoke"},
  "extends": "default",
  "filesystem": {
    "read": ["/absolute/workspace", "/absolute/binary-directory"],
    "allow": ["/absolute/workspace/smoke-output", "/absolute/data", "/absolute/scratch"]
  },
  "workdir": {"access": "read"},
  "network": {"block": false}
}
```

Create the named directories first; run `nono profile validate` and launch from
the explicitly granted workspace. The ACP client must pass stdio pipes and
supply its usual provider environment. The flags above select compatibility
testing without nono audit/rollback/proxy services; a real user's policy needs
its own review. ACP permission remains in effect independently of nono.

The wrapper gives the agent and its ordinary descendants the session policy.
It does not itself create a separate bash-to-agent boundary. Dynamic workspace
roots, remote HTTP MCP behavior, proxy certificates, raw-key protection against
hostile shell inspection, detached descendants and parent-death cleanup remain
outside this result. ADRs 0025/0026 and the native-backend no-go remain in force.

## Cleanup and repository validation

The original T3 executable setting was restored to `acplause`; its secret
configuration was not replaced. Test-owned provider processes, database,
launchers, metadata, workspace files and nono state/config directories were
removed. No host trust-store/security setting or installed binary was changed.

`just check` passed after recording this result. No production source,
dependency or protocol schema changed.
