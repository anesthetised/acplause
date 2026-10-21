# ADR 0024: Calendar releases and source-based Homebrew distribution

- Status: accepted
- Date: 2026-10-21

## Context

acplause follows a draft ACP v2 protocol and needs a simple installation and
upgrade path. There is no paid Apple Developer ID certificate. Releases must
be explicitly initiated and recover safely from partial failures.

## Decisions

- Publish ordinary GitHub Releases from a manual workflow on `main`. Keep
  experimental status visible in README. Use `v0.YYYYMM.N`, with the UTC
  calendar month and a zero-based monthly counter. Major zero preserves the
  current Go module path. These calendar versions do not promise compatibility;
  breaking changes are called out explicitly.
- Use GoReleaser to package four CGO-free binaries (macOS/Linux, arm64/amd64)
  and a source archive. Publish SHA256 checksums and free GitHub artifact
  attestations. Docker distribution is deferred.
- Use git-cliff, based on couchcast's configuration, for detailed notes after
  the first release. Keep breaking changes visible and internal work collapsed.
  The first release body is empty.
- Install from source through `anesthetised/homebrew-tap`. Go is a build-only
  dependency. No cask, quarantine-removal hook, or paid Apple signing is needed
  for this path; downloaded macOS binaries remain unsigned by Developer ID.
- Stamp the complete tag, including `v`, and source commit into builds. Use
  that version in CLI diagnostics, ACP initialization and the provider
  User-Agent. Source-archive builds receive the same metadata explicitly.
- Save the release plan and exact bundle as immutable Actions artifacts before
  publication. Use GitHub CLI for incremental draft uploads and publication,
  so retries do not depend on reproducing bytes. Never move tags or replace
  published assets. Update the tap after publication with a separate scoped
  PAT; preserve the maintainer's Git identity and Conventional Commits.

## Consequences

Homebrew installations take longer but avoid unsigned download launch friction.
GoReleaser is used for packaging; a small Python standard-library coordinator
owns version allocation, retry checks and the source formula, rather than its
deprecated binary-formula publisher. Recovery artifacts expire after 90 days;
older interrupted releases require manual investigation. Attestations prove
origin, not safety or Apple notarization. Reproducible builds are not required.

See [release operations](../releases.md) for the exact workflow and recovery path.
