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

**This is enforced, not advisory.** CI fails a PR that changes Go source without
touching `CHANGELOG.md`, and `changelog_test.go` checks `[Unreleased]` for duplicate
group headings, unknown group names, entries outside a group, and releases missing a
compare link. Both failure modes had already happened before the gate existed.

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

## The public surface (other repos compile against it)

Most of `pkg/` is internal by convention. **These eight packages are not** — they
are imported by other repositories, so an exported symbol in them is a wire
contract:

| package | imported by |
|---|---|
| `pkg/aws` | spore-host-mcp, lagotto, calque |
| `pkg/launcher` | spore-host-mcp, lagotto, calque |
| `pkg/taskproto` | spore-host-mcp |
| `pkg/ecrref` | spore-host-mcp |
| `pkg/launchererr` | lagotto |
| `pkg/taskcohort` | calque |
| `pkg/taskpool` | calque |
| `pkg/storage` | calque |

**Removing an exported symbol, renaming one, or changing a signature or an
exported struct field in any of them bumps MINOR, never PATCH** — pre-1.0 that is
this repo's rule for a breaking change. Adding one is backward-compatible.

This is enforced. `api/public-surface.txt` is a committed snapshot of that
surface and `api_surface_test.go` fails CI when it drifts, telling you whether
what you changed was an addition or a removal. When the change is intended, run
`make api-snapshot` and commit the result **in the same PR**, and say so in the
CHANGELOG if anything was removed or changed.

Why it is enforced rather than advised: `pkg/taskproto.GenerateWrapper` gained a
parameter in **v0.111.1, a PATCH release** (#679). A consumer is entitled to
treat a patch bump as safe, and Dependabot is configured across these repos on
exactly that assumption — so the breakage surfaced as a red build in
spore-host-mcp that sat for six days with nothing linking it back here.

Keep the table and the test's `publicPackages` map in step; the map is the one CI
reads.

## The deployed control plane

`spawn` is not only a CLI: 18 Lambda functions, 23 DynamoDB tables, 21 S3
buckets and 16 IAM roles live in the shared infra account (966362334030).
**[docs/infra-account.md](docs/infra-account.md)** maps what is there to the code
that owns it, how each function is invoked, and the deploy traps that have each
cost a real failure.

Read it before changing anything under `lambda/`. It is measured from the live
account rather than recalled — re-measure with `spawn footprint` and
`make lambda-versions-deployed` rather than trusting the page.

Two things it records that are easy to get wrong: nothing reaps the control plane
(the TTL reaper covers instances, not this), and several live functions are
deployed from *other* repos, so an unfamiliar function name is not necessarily an
orphan.

## Writing a gate

A lot of this repo's CI is **gates**: tests whose job is to fail when someone
removes a property, rather than to check that today's code works. `make check`
runs dozens of them. They are cheap to write and easy to write *uselessly* — a
gate that cannot fail is worse than no gate, because it is counted as coverage.

Four rules, each of which exists because a gate here broke one of them:

1. **A gate is not done until you have watched it fail against a revert that
   compiles.** "I reverted the change and the test failed" is only evidence if
   the test *ran*. Three gates in one session were "verified" against reverts
   that failed to build — `go vet` stopped at an unused variable or a dropped
   import, so the gate never executed and the verification proved nothing. Make
   the revert compile (`_ = x`, `case false:`) and then watch the gate fail.

2. **Test the invariant where it lives, not only through the path that reaches
   it.** A guard can be unreachable through its own constructor and still be
   load-bearing for a future caller. `Idleness.IdleFor`'s `!Determined` check
   survived a deliberate revert because every test built the struct through
   `DetectIdleness`, which never produces the offending combination (#787). The
   fix was a test that constructs the invalid state directly.

3. **Match the assignment or the invocation, never the bare name.** Four gates
   matched their own explanation: `spawn:version` appeared in a CloudFormation
   parameter `Description` and in an `echo`, and `put-retention-policy` appeared
   in a comment saying which call to make. The working forms are anchored and
   structural — `^\s*spawn:version:\s*!Ref\s+Version`, `--tags.*spawn:version=`,
   `^\s*aws logs put-retention-policy` (see `scripts/lambda-deploy-census.sh`).
   Match the identifier on its own and the sentence documenting the gate will
   satisfy it.

4. **Discover the set, don't list it.** A gate over a hand-written list reports
   a clean bill of health for everything the list forgot. The first version of
   `scripts/lambda-deploy-census.sh` named five functions in an account holding
   more than twice that many; three control-plane inventories in #653 were each
   incomplete; and a log-group query filtered by prefix found 10 of 14 groups,
   missing the one holding 80% of the data. Enumerate from the filesystem or the
   API, and **fail when discovery finds nothing** — `scripts/nested-modules.sh`
   exits non-zero on an empty result, because a discovery that found nothing has
   broken, not proven the repo clean. Where a list is unavoidable
   (`footprintExactNames` in `pkg/aws/footprint.go`, for names carrying no
   prefix), add an entry only after seeing it in a live account, never from
   memory.

One corollary, for when a gate reports an absence: **a suspicious negative is
more often the invocation than the system.** Four false negatives in one session
came from the tool, not the code under test — `strings` on a Go binary, `awk $2`
on tab-separated `gh pr checks` output, `--max-items` reshaping a JMESPath
result to `null`, and `FilterLogEvents --limit` returning an empty *first* page
with a continuation token. The last nearly produced a "fix" to working code.
Confirm an absence by a second, differently-shaped route before acting on it.

## Build & test

- `make check` — fmt, vet, lint, short tests, and every nested module (run
  before every commit)
- `make test` — full unit tests with coverage
- `make build` — build spawn + spored

## Cost safety

This tool launches real, billable EC2 instances. Any real-AWS test MUST set a
TTL, terminate explicitly when done, and independently leak-check (no orphaned
instances) afterward. Cost control is existential to the project.
