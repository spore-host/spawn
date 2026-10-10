#!/usr/bin/env bash
#
# Build, vet and test every nested Go module.
#
# Why this exists at all (#136): the root `go test ./...` does NOT descend into a
# directory with its own go.mod, so the 12 modules under lambda/ and scripts/ are
# invisible to it. Their tests would never run.
#
# Why it is a SCRIPT rather than steps in two places (#772's aftermath). CI ran
# this loop and `make check` did not, so the local gate was weaker than the remote
# one — and that gap cost two round trips in one session:
#
#   #770  LogRetentionDays was added to three templates and wired into none.
#         ttl-reaper's own test caught it, and only CI runs that test.
#   #771  pkg/aws gained six SDK imports; sweep-orchestrator needed re-tidying
#         and failed on a missing go.sum entry.
#
# Both were found by CI after a push, by which point the fix is a second commit
# and another wait. A local gate that differs from CI teaches people to push and
# find out, which is slower for everyone. One script, called from both, cannot
# drift.
#
# Usage: scripts/nested-modules.sh
#
# There is deliberately no --quiet. The first version had one documented as "only
# report failures", which it did not do — go test writes its own output and the
# flag only suppressed this script's headers. A flag whose name overstates what it
# does is worse than no flag.
set -euo pipefail

# Discovered, not listed, and deliberately NOT scoped to lambda/*: that scoping
# was the #136 bug one level up, and it silently excluded scripts/ on the day that
# module was added. A list of modules is a list someone has to remember to
# update — the same failure mode as the hardcoded function names in
# lambda-deploy-census.sh and the three incomplete footprint inventories in #653.
mods=$(find . -mindepth 2 -name go.mod -not -path './.git/*' | sort)
if [ -z "$mods" ]; then
  # Refusing to pass vacuously. A discovery that finds nothing has failed; it has
  # not proven the repo clean.
  echo "found no nested modules — the discovery is broken, not the repo" >&2
  exit 1
fi

# GitHub's log-folding markers, only when running in Actions. Locally they would
# just be noise.
group_open() { [ -n "${GITHUB_ACTIONS:-}" ] && echo "::group::$1" || echo "  $1"; return 0; }
group_close() { [ -n "${GITHUB_ACTIONS:-}" ] && echo "::endgroup::"; return 0; }

fail=0
for mod in $mods; do
  dir=$(dirname "$mod")
  group_open "$dir"
  if ! ( cd "$dir" && go build ./... && go vet ./... && go test -short ./... ); then
    echo "FAIL $dir" >&2
    fail=1
  fi
  group_close
done

if [ "$fail" -eq 0 ]; then
  echo "✅ all $(printf '%s\n' "$mods" | grep -c .) nested modules build, vet and test clean."
fi
exit $fail
