#!/usr/bin/env bash
#
# Fail if any Go module in the repo — the root or a nested one — is not tidy.
#
# Why (#907): pkg/integration/orchestrator_test.go sat behind
# `//go:build integration` importing pkg/orchestrator for days after #862
# deleted that package. Nothing in `make check` or CI compiles that tag, so
# the dead import was invisible to build, vet and test alike. `go mod tidy`
# is the one tool that loads every package under every build tag, so it was
# the only thing that could see it — and nothing ran it. Meanwhile go.mod
# drifted (robfig/cron kept as a direct requirement of deleted code, pflag
# mislabelled indirect), and PR #900 had to hand-edit go.mod because tidy
# could not run at all.
#
# `go mod tidy -diff` changes nothing and exits non-zero both when tidy would
# rewrite go.mod/go.sum and when tidy cannot run (an unresolvable import),
# so it gates both failure modes.
#
# Usage: scripts/check-tidy.sh
set -euo pipefail

# Discovered, not listed — the same discovery scripts/nested-modules.sh uses,
# plus the root module itself.
mods=$(find . -name go.mod -not -path './.git/*' | sort)
if [ -z "$mods" ] || ! printf '%s\n' "$mods" | grep -qx './go.mod'; then
  # Refusing to pass vacuously: if discovery missed the root module it has
  # broken, not proven the repo clean.
  echo "module discovery did not find ./go.mod — the discovery is broken, not the repo" >&2
  exit 1
fi

fail=0
for mod in $mods; do
  dir=$(dirname "$mod")
  if ! out=$(cd "$dir" && go mod tidy -diff 2>&1); then
    echo "::error::$dir is not tidy — run 'go mod tidy' in $dir and commit the result:" >&2
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then
  echo "✓ all $(printf '%s\n' "$mods" | grep -c .) modules are tidy"
fi
exit $fail
