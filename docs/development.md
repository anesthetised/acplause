# Development and testing

[Documentation](README.md) · [Project overview](../README.md)

```sh
just check   # gofmt, vet, staticcheck, tests with -race, go mod tidy
just e2e -v  # end-to-end tests only
```

GitHub Actions runs `just check` on Linux on every pull request and push to
`main`. A native Windows amd64 job runs `go vet` and all unit/protocol e2e tests
with the race detector, then builds CGO-free amd64 and arm64 binaries. It also
cross-compiles arm64 test packages without executing them; native arm64 and
real-client Windows interoperability still require their own runtime checks.
The checks also need Python 3 and git-cliff 2.14.2 for release automation tests.
CI reads the Go version from `go.mod` and pins just 1.59.0 and Staticcheck
2026.2.2 (v0.8.2).

CI collects Go statement coverage in the same test run, fails below 80%, and
publishes a shields.io endpoint file to the `badges` branch after successful
pushes to `main`, using only `GITHUB_TOKEN`. Coverage measures code run in the
test processes; the binary built separately by the e2e harness is not included.

The end-to-end tests in `e2e/` build the binary, drive it over stdio against
a scripted SSE server and validate every message it writes against the ACP
schema. Decisions are recorded in [architecture decisions](adr).

Notes for coding agents are in [AGENTS.md](../AGENTS.md).

The opt-in macOS [Seatbelt prototype](testing/seatbelt-prototype.md) runs with
`just seatbelt-prototype`. Its failed gates block runtime integration; the
ordinary CI suite does not run these host-dependent experiments.

See [releases](releases.md) for publishing, recovery and local packaging.

## Windows development

Use Windows PowerShell and the Go version in `go.mod`. `just build` produces
`acplause.exe`; `just test` and `just e2e -v` run the same portable behavior and
ACP schema assertions as Unix. The shell-tool tests select Windows PowerShell
(`powershell.exe`) on Windows and Bash on Unix. Unix signal/process-group tests
remain in Unix-tagged files; Windows lifecycle tests exercise Job Objects.

The race detector on Windows amd64 requires CGO and a C compiler with MinGW-w64
runtime 8 or later. CI verifies that `gcc --print-file-name=libsynchronization.a`
resolves to an existing library before running the tests. See the
[Go race detector requirements](https://go.dev/doc/articles/race_detector).
Windows arm64 does not support the race detector; use `go test -count=1 ./...`
on native arm64. Release builds use `CGO_ENABLED=0`.

Symlink fixtures require Developer Mode or the Windows symbolic-link privilege.
Only tests requiring an unavailable symlink privilege are skipped; absolute
paths, additional roots and workspace escape rejection still run. File-edit
tests preserve the permissions exposed by the host filesystem. Unix-only
owner mode checks do not claim to verify Windows ACLs.

`just check` also requires Staticcheck, Python 3 (`python` on Windows), and
git-cliff for release automation tests. The Windows CI job runs the Go checks;
Linux retains the complete release-test and >=80% statement coverage gates.
