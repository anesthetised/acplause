# Development and testing

[Documentation](README.md) · [Project overview](../README.md)

```sh
just check   # gofmt, vet, staticcheck, tests with -race, go mod tidy
just e2e -v  # end-to-end tests only
```

GitHub Actions runs `just check` on every pull request and push to `main`.
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
