#!/usr/bin/env bash
#
# Report whether changes since a baseline touch a hardware-sensitive path.
#
# This does not block anything. It answers one question an operator currently has
# to answer from memory at the worst possible moment — tagging time — and gets
# wrong, because the suite is green and the diff looks fine.
#
# The premise is narrow and evidence-based: for the paths in
# scripts/hardware-sensitive.txt, a green unit suite is not evidence of
# correctness, because the properties that break are properties of a running
# instance. v0.118.0 shipped eleven fixes; four of them were findable only on
# hardware, and three pre-existing tests asserted broken behaviour as correct.
#
# Usage:
#   hardware-smoke-needed.sh                 # since the last tag
#   hardware-smoke-needed.sh origin/main     # since a ref
#   hardware-smoke-needed.sh --exit-code     # exit 1 if a smoke is needed
set -uo pipefail

manifest="$(dirname "$0")/hardware-sensitive.txt"
[ -f "$manifest" ] || { echo "missing $manifest" >&2; exit 2; }

exit_code=0
base=""
for arg in "$@"; do
  case "$arg" in
    --exit-code) exit_code=1 ;;
    *) base="$arg" ;;
  esac
done

if [ -z "$base" ]; then
  base=$(git describe --tags --abbrev=0 2>/dev/null) || {
    echo "no tags yet and no baseline given" >&2; exit 2; }
fi

# Test files are excluded: changing a test cannot change what an instance does.
# Only the shipped code can, and conflating the two makes the signal noisy enough
# to start ignoring — which is the failure mode this is meant to prevent.
# Resolve the diff FIRST, so a git failure is distinguishable from "no matches".
# With `set -o pipefail`, piping straight into grep makes grep's exit 1 (nothing
# left after filtering) look like a git error — which it did, reporting "cannot
# diff" immediately after a successful tag.
if ! all_changed=$(git diff --name-only "$base"...HEAD 2>/dev/null); then
  echo "cannot diff against '$base'" >&2; exit 2
fi
# Test files are excluded: changing a test cannot change what an instance does.
# Only the shipped code can, and conflating the two makes the signal noisy enough
# to start ignoring — which is the failure mode this is meant to prevent.
changed=$(printf '%s\n' "$all_changed" | grep -v '_test\.go$' || true)
[ -n "$changed" ] || { echo "No changes since ${base}."; exit 0; }

hits=0
printf '%s\n' "Changes since ${base} touching hardware-sensitive paths:" >&2
while IFS=$'\t' read -r prefix why; do
  case "$prefix" in ''|'#'*) continue ;; esac
  matched=$(printf '%s\n' "$changed" | grep -F "$prefix" || true)
  [ -n "$matched" ] || continue
  hits=$((hits + 1))
  printf '\n  %s\n' "$prefix" >&2
  printf '    why: %s\n' "$why" >&2
  printf '%s\n' "$matched" | sed 's/^/    changed: /' >&2
done < "$manifest"

if [ "$hits" -eq 0 ]; then
  echo "" >&2
  echo "✅ No hardware-sensitive paths changed since ${base}; unit coverage is the whole story." >&2
  exit 0
fi

cat >&2 <<MSG

⚠️  ${hits} hardware-sensitive area(s) changed. Run 'make smoke' before tagging.

    A green unit suite does not settle these. The smoke is ~10 minutes and a few
    cents; a wrong answer here has shipped a non-functional --mpi, an instance
    leak, and a disarmed production reaper.
MSG

[ "$exit_code" -eq 1 ] && exit 1
exit 0
