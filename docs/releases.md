# Releases

[Documentation](README.md) · [Decision](adr/0024-release-distribution.md)

## Publish

Open **Actions → Release → Run workflow**, select **main**, and run it. Or run
`just release` with an authenticated GitHub CLI; this dispatches the same
workflow on GitHub, without compiling anything locally.

The workflow fixes the source commit at dispatch time and chooses
`v0.YYYYMM.N` using the run's creation month in UTC. `N` starts at zero and
increases within the month. It runs `just check`, enforces 80% Go coverage,
packages macOS/Linux binaries for arm64/amd64 with GoReleaser, creates SHA256
checksums and GitHub artifact attestations, then publishes the tag and ordinary
GitHub Release. Finally it updates `Formula/acplause.rb` in
[homebrew-tap](https://github.com/anesthetised/homebrew-tap).

The first release has an empty body. Later bodies come from `cliff.toml`:
breaking changes first, then features, fixes, performance, documentation and
build/dependencies. Refactoring, tests, CI and other changes appear in a
collapsible Internal changes section. Use Conventional Commits, including
`!` or `BREAKING CHANGE:` when required; describe migrations in that footer.
Release bookkeeping and merge commits are excluded, except breaking changes.

The tap uses the `HOMEBREW_TAP_TOKEN` Actions secret: a fine-grained PAT scoped
only to `anesthetised/homebrew-tap`, with Contents read/write. Renew it before
expiration. It is only exposed to the tap update step. Source release writes
use the repository's `GITHUB_TOKEN`. Tap commits retain the maintainer's Git
name and email and use a Conventional Commit subject.

## Retry a failed run

Use **Re-run failed jobs** on the original run. **Re-run all jobs** also keeps
the version and source commit. The `release-plan` artifact saves the selected
version before building; `release-bundle` saves the exact archives, notes and
formula before any public tag is created. Both are retained for 90 days.
The workflow never moves a tag or replaces uploaded bytes. A partial draft
release receives only its missing assets and becomes public after completion.
If the tap fails, the release remains available and retry updates only the tap.

Do not dispatch another release to recover an unfinished one. A newer version
blocks publication of an older draft and prevents tap downgrades. A missing
or expired recovery artifact requires manual investigation; the workflow fails
instead of guessing a new version. If a run failed before saving its plan,
verify that no tag/release was created and start a new run.

## Install and verify

```sh
brew install anesthetised/tap/acplause
brew upgrade acplause
acplause --version
```

Homebrew compiles the source archive locally with Go as a build dependency.
It stamps the same full tag and source commit as the binary archives. This
avoids distributing a macOS binary that needs paid Developer ID signing or
an installation hook to remove quarantine. It costs compilation time.

For a downloaded archive, get `checksums.txt` from the same release. Run
`shasum -a 256 <archive>` and compare its entry, then verify provenance with:

```sh
gh attestation verify <archive> --repo anesthetised/acplause
```

Attestations establish the Actions build's origin; they are not Apple
notarization. Prebuilt macOS binaries may still need explicit launch approval.
Byte-for-byte reproducible builds are not a release requirement.

`acplause --version` and `doctor` write build details to stderr. ACP
`initialize` reports the full version, and provider requests, including the
online doctor probe, send `User-Agent: acplause/v0.YYYYMM.N`. OpenRouter's
application title remains `acplause`. Ordinary Go builds fall back to Go's
embedded module/VCS metadata or `dev` when unavailable.
