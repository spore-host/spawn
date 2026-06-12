# CLAUDE.md — spawn

`spawn` is the spore.host tool for launching and managing AWS EC2 instances
(Linux and Windows), including the `spored` in-instance lifecycle daemon. Part of
the spore.host suite ([truffle](https://github.com/spore-host/truffle),
[lagotto](https://github.com/spore-host/lagotto), spawn).

## Versioning & changelog (required)

This project follows **[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)**
and keeps a **[Keep a Changelog](https://keepachangelog.com/en/1.1.0/)**-format
`CHANGELOG.md` at the repo root.

**Every change that affects users must update `CHANGELOG.md`:**

- Add an entry under the `## [Unreleased]` section, in the right group —
  `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, or `Security`. (Use a
  `Documentation` group for docs-only changes; these are optional but welcome.)
- Write for humans: describe the user-visible effect, not the implementation.
  Reference the issue/PR where it helps.
- Do this in the **same PR** as the change, so the changelog never lags.

**On release:**

1. Rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD` and open a fresh empty
   `## [Unreleased]` above it.
2. Choose `X.Y.Z` by SemVer: **MAJOR** for breaking changes, **MINOR** for
   backward-compatible features, **PATCH** for backward-compatible fixes. (Pre-1.0,
   breaking changes bump MINOR.)
3. Update the comparison links at the bottom of the file.
4. Tag `vX.Y.Z` — that triggers the GoReleaser release workflow.

GoReleaser auto-generates the **GitHub Release notes** from commit messages;
`CHANGELOG.md` is the curated, human-facing companion and is the source of truth
for "what changed." Keep both — they serve different readers.

## Build & test

- `make check` — fmt, vet, lint, short tests (run before every commit)
- `make test` — full unit tests with coverage
- `make build` — build spawn + spored

## Cost safety

This tool launches real, billable EC2 instances. Any real-AWS test MUST set a
TTL, terminate explicitly when done, and independently leak-check (no orphaned
instances) afterward. Cost control is existential to the project.
