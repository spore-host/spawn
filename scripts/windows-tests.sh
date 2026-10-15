#!/usr/bin/env bash
#
# Run the root module's unit tests on Windows (#971).
#
# spawn ships a Windows spored (pkg/agent/sys_windows.go, cmd/spored/*_windows.go)
# but CI ran only on Linux, so Windows-only tests were compiled at best and never
# executed. This is what the windows-latest CI job runs.
#
# Every package is tested EXCEPT those in scripts/windows-test-skips.txt, each of
# which carries a reason and an issue. Inverted on purpose: an allowlist would
# silently leave every new package untested on Windows; a skip list makes the
# exclusion the thing someone has to write down.
#
# No AWS: `-short`, and the job sets no credentials. A test that reaches AWS
# without them fails here exactly as it would on Linux.
#
# Usage: scripts/windows-tests.sh [extra go test flags...]
set -euo pipefail
cd "$(dirname "$0")/.."

skips=scripts/windows-test-skips.txt
mod=$(go list -m)

declare -A skip=()
while IFS= read -r line || [ -n "$line" ]; do
  line=${line%$'\r'}
  case "$line" in ''|'#'*) continue ;; esac
  pkg=${line%%[[:space:]]*}
  skip["$mod/$pkg"]=1
done < "$skips"

pkgs=()
while IFS= read -r p; do
  p=${p%$'\r'}
  if [ -n "${skip[$p]:-}" ]; then
    echo "skip $p ($(grep -E "^${p#"$mod"/}[[:space:]]" "$skips" | sed -E 's/^[^[:space:]]+[[:space:]]+//'))"
    continue
  fi
  pkgs+=("$p")
done < <(go list ./...)

if [ "${#pkgs[@]}" -eq 0 ]; then
  # A discovery that found nothing has broken; it has not proven Windows clean.
  echo "found no packages to test — the discovery is broken" >&2
  exit 1
fi

echo "testing ${#pkgs[@]} packages on $(go env GOOS)/$(go env GOARCH)"
go test -short -count=1 "$@" "${pkgs[@]}"
