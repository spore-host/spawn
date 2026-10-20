#!/usr/bin/env bash
#
# Run golangci-lint over every Go module in the repo: the root and each nested
# one (#1028).
#
# Why every module: `golangci-lint run ./...` stops at a directory with its own
# go.mod, exactly like `go test ./...` (#136), so linting only the root would
# leave the nine modules under lambda/ and scripts/ — the ttl-reaper among them
# — unlinted.
#
# Every module is linted against the ROOT .golangci.yml, passed explicitly, so a
# nested module cannot pick up a config of its own and quietly lint less. That
# file is a ratchet: see its header and docs/lint-ratchet.md.
#
# Usage: scripts/lint.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
config="$root/.golangci.yml"

# Discovered, not listed — the same discovery scripts/check-tidy.sh uses.
mods=$(find . -name go.mod -not -path './.git/*' | sort)
if ! printf '%s\n' "$mods" | grep -qx './go.mod' ||
   [ "$(printf '%s\n' "$mods" | grep -cvx './go.mod')" -eq 0 ]; then
  # Refusing to pass vacuously: a discovery that missed the root, or found no
  # nested module in a repo that has nine, has broken, not proven the repo clean.
  echo "module discovery found $(printf '%s\n' "$mods" | grep -c .) go.mod file(s) — the discovery is broken, not the repo" >&2
  exit 1
fi

group_open() { [ -n "${GITHUB_ACTIONS:-}" ] && echo "::group::$1" || echo "  $1"; return 0; }
group_close() { [ -n "${GITHUB_ACTIONS:-}" ] && echo "::endgroup::"; return 0; }

fail=0
for mod in $mods; do
  dir=$(dirname "$mod")
  group_open "$dir"
  if ! ( cd "$dir" && golangci-lint run --config "$config" ./... ); then
    echo "FAIL $dir" >&2
    fail=1
  fi
  group_close
done

if [ "$fail" -eq 0 ]; then
  echo "✓ golangci-lint clean in all $(printf '%s\n' "$mods" | grep -c .) modules"
fi
exit $fail
