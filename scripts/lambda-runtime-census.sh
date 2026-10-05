#!/usr/bin/env bash
#
# Fail if any Lambda runtime — declared in this repo, or deployed in an account
# we own — is not on the approved list in scripts/lambda-runtimes.txt.
#
# Why this exists (spawn#716). AWS ending support for Python 3.8 raised the
# question "are we on a supported runtime anywhere?", and answering it took
# `lambda list-functions` across two accounts and two regions by hand. That
# turned up two runtimes already stale since January, one of which is not in any
# repository. The gap is not that a runtime was old; it is that nothing could
# have told us.
#
# Two halves, deliberately, mirroring scripts/verify-pins.sh:
#
#   offline   greps every runtime DECLARATION in the repo. No credentials, so it
#             can gate every PR.
#   networked lists DEPLOYED functions and checks them too, because a function
#             declared nowhere — github-oauth-bridge — is invisible to the
#             offline half by definition.
#
# Usage:
#   lambda-runtime-census.sh                      # offline only
#   lambda-runtime-census.sh --deployed           # offline + the accounts below
#   AWS_PROFILE=x REGIONS="us-east-1 us-west-2" lambda-runtime-census.sh --deployed
set -uo pipefail

manifest="$(dirname "$0")/lambda-runtimes.txt"
[ -f "$manifest" ] || { echo "missing $manifest" >&2; exit 2; }

# Approved runtimes, one per line, comments and blanks stripped.
approved=$(grep -vE '^\s*#|^\s*$' "$manifest" | cut -f1 | tr -d ' ')
[ -n "$approved" ] || { echo "no approved runtimes parsed from $manifest — the matcher is broken" >&2; exit 2; }

is_approved() { printf '%s\n' "$approved" | grep -qxF "$1"; }

fail=0
note() { printf '  %s\n' "$1" >&2; }

# ---------------------------------------------------------------------------
# Offline: every runtime declaration in the repo.
#
# THREE forms, not one. Scanning only `Runtime:` in templates is what let
# provided.al2 sit in scripts/deploy-scheduler-handler.sh unnoticed — a shell
# deploy sets it with --runtime, and a grep for the YAML key never sees it.
# ---------------------------------------------------------------------------
root="$(cd "$(dirname "$0")/.." && pwd)"

# runtimes_in <label> <pattern> <include globs...> — prints "<runtime>\t<label>".
#
# NOTE the option order: every --include comes BEFORE the pattern. Writing
# `grep -rhoE -- 'pat' "$root" --include='*.sh'` makes the include a FILE
# argument instead of an option, because `--` ends option parsing — so grep
# silently scans everything. That bug was in the first version of this script and
# it is how a python3.11 in a README turned up while the "where is it" grep,
# which had the includes in the right place, reported nothing. A matcher that
# scans more than it claims is as misleading as one that scans less.
runtimes_in() {
  local label="$1" pattern="$2"; shift 2
  local includes=()
  local g
  for g in "$@"; do includes+=("--include=$g"); done
  grep -rhoE "${includes[@]}" -e "$pattern" "$root" 2>/dev/null \
    | sed -E "s/$3//" | sed -E 's/^.*(Runtime: *|--runtime +)//' \
    | while IFS= read -r rt; do [ -n "$rt" ] && printf '%s\t%s\n' "$rt" "$label"; done
}

# Three places a runtime is declared, and docs count.
#
# Scanning only `Runtime:` in templates is what let provided.al2 sit in
# scripts/deploy-scheduler-handler.sh unnoticed: a shell deploy sets it with
# --runtime and the YAML key never appears. Documentation counts for a different
# reason — examples/workflows/step-functions/README.md told users to deploy with
# python3.11, and published advice is acted on, so a stale runtime there reaches
# further than one in an internal script.
declared=$(
  {
    grep -rhoE --exclude='lambda-runtime-census.sh' --exclude='lambda-runtimes.txt' --include='*.yaml' --include='*.yml' --include='*.json' \
      -e 'Runtime: *[a-z][a-z.]*[0-9][a-z0-9.]*' "$root" 2>/dev/null \
      | sed -E 's/Runtime: *//' | sed 's/$/\ttemplate/'
    grep -rhoE --exclude='lambda-runtime-census.sh' --exclude='lambda-runtimes.txt' --include='*.sh' \
      -e '--runtime +[a-z][a-z.]*[0-9][a-z0-9.]*' "$root" 2>/dev/null \
      | sed -E 's/--runtime +//' | sed 's/$/\tdeploy script/'
    grep -rhoE --exclude='lambda-runtime-census.sh' --exclude='lambda-runtimes.txt' --include='*.md' \
      -e '--runtime +[a-z][a-z.]*[0-9][a-z0-9.]*' "$root" 2>/dev/null \
      | sed -E 's/--runtime +//' | sed 's/$/\tdocumentation/'
  } | sort -u
)

if [ -z "$declared" ]; then
  echo "found no runtime declarations at all — the matcher is stale and this check" >&2
  echo "would pass vacuously, which is the failure mode it exists to prevent" >&2
  exit 2
fi
# Stronger than "found something": assert the runtime we KNOW is declared is
# among the findings. An earlier pattern here matched python3.11 but not
# provided.al2023, so the emptiness guard above passed while the check had gone
# half-blind — which is exactly the shape of failure it was meant to catch.
if ! printf '%s\n' "$declared" | cut -f1 | grep -qxF 'provided.al2023'; then
  echo "the matcher did not find provided.al2023, which every Go lambda here" >&2
  echo "declares — it is broken, and a pass would be meaningless" >&2
  exit 2
fi

echo "Declared runtimes in $(basename "$root"):" >&2
while IFS=$'\t' read -r rt where; do
  [ -n "$rt" ] || continue
  if is_approved "$rt"; then
    note "✅ $rt ($where)"
  else
    fail=1
    note "❌ $rt — NOT approved, in $where:"
    grep -rnE --exclude='lambda-runtime-census.sh' --exclude='lambda-runtimes.txt' --include='*.yaml' --include='*.yml' --include='*.json' \
      --include='*.sh' --include='*.md' \
      -e "Runtime: *$rt" -e "--runtime +$rt" "$root" 2>/dev/null \
      | sed "s|$root/||" | sed 's/^/       /' >&2
  fi
done <<< "$declared"

# ---------------------------------------------------------------------------
# Networked: what is actually running.
# ---------------------------------------------------------------------------
if [ "${1:-}" = "--deployed" ]; then
  command -v aws >/dev/null 2>&1 || { echo "aws CLI not found" >&2; exit 2; }
  regions="${REGIONS:-us-east-1 us-west-2}"
  echo "" >&2
  echo "Deployed runtimes (profile ${AWS_PROFILE:-default}):" >&2

  found_any=0
  for region in $regions; do
    # name<TAB>runtime, one per line.
    listing=$(aws lambda list-functions --region "$region" \
      --query 'Functions[].[FunctionName,Runtime]' --output text 2>/dev/null)
    [ -n "$listing" ] || continue
    while IFS=$'\t' read -r fn rt; do
      [ -n "$fn" ] || continue
      found_any=1
      if is_approved "$rt"; then
        continue
      fi
      fail=1
      note "❌ $fn ($region) runs $rt — NOT approved"
      # Is it even in the repo? A function nothing declares cannot be fixed by
      # editing a template, and that distinction is the actionable part.
      # Filter self-references by PATH rather than with grep --exclude.
      #
      # This script and its manifest name the offending functions in their
      # comments, so without filtering the census matches its OWN documentation
      # and reports an un-sourced function as "referenced in the repo" —
      # inverting the one line that says what to do about it. --exclude with a
      # bare basename is not honoured by the BSD grep on macOS, so relying on it
      # produced exactly that wrong answer; filtering the result list is
      # deterministic across greps.
      refs=$(grep -rlF --include='*.yaml' --include='*.yml' --include='*.json' \
               --include='*.sh' --include='*.go' -e "$fn" "$root" 2>/dev/null \
             | grep -vE '/scripts/lambda-runtime-(census\.sh|runtimes\.txt)$' \
             | grep -vE '/scripts/lambda-runtimes\.txt$')
      if [ -n "$refs" ]; then
        note "     referenced in the repo — bump the declaration and redeploy"
        printf '%s\n' "$refs" | sed "s|$root/||" | sed 's/^/       /' >&2
      else
        note "     NOT in this repo at all — it cannot be rebuilt, reviewed or"
        note "     rolled back. Import it or retire it."
      fi
    done <<< "$listing"
  done

  if [ "$found_any" = "0" ]; then
    echo "  no Lambdas found in: $regions — wrong profile, or nothing deployed?" >&2
    echo "  (not treated as a pass; check the profile before believing this)" >&2
  fi
fi

echo "" >&2
if [ "$fail" = "0" ]; then
  echo "✅ every runtime checked is on the approved list." >&2
  exit 0
fi

cat >&2 <<'MSG'
❌ Unapproved Lambda runtime(s).

Either bump the runtime, or — if it is genuinely approved now — add it to
scripts/lambda-runtimes.txt with the reason. Editing that file is the deliberate
act; finding an unsupported runtime in production is not.
MSG
exit 1
