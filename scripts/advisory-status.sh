#!/usr/bin/env bash
# Security-advisory step of the release ritual (CLAUDE.md "On release", #828).
#
#   scripts/advisory-status.sh [vX.Y.Z]   # default: the newest v* tag
#
# Read-only: it lists the repository's advisories and changes nothing. Setting
# patched_versions and publishing are done by hand, and publishing is the
# owner's decision. Not a CI gate, because listing advisories (drafts included)
# needs a token with access to them.
#
# Per advisory it prints the state, the vulnerable range and patched_versions,
# and flags two mistakes:
#   STALE     published, patched_versions empty, but RELEASE is outside the
#             vulnerable range: a fixed release exists, so the empty field has
#             gone from honest to stale.
#   NO-TAG    patched_versions names a version with no tag here: it claims a
#             fix in a release that does not exist.
# It cannot tell whether an advisory's fix is in a release. Check that with
#   git merge-base --is-ancestor <fix-commit> vX.Y.Z^{}
# before setting patched_versions.
#
# Exit status: 0 if nothing is flagged, 1 if something is, 2 on a usage or
# discovery error.
set -euo pipefail

repo=${REPO:-spore-host/spawn}
release=${1:-$(git tag --list 'v[0-9]*' --sort=-v:refname | head -n1)}
release=${release#v}
if [ -z "$release" ]; then
  echo "advisory-status: no v* tag found; pass the release as vX.Y.Z" >&2
  exit 2
fi

# ver_le A B: A <= B by version order.
ver_le() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" = "$1" ]; }

# in_range VERSION RANGE: whether VERSION satisfies every comma-separated
# clause of a GitHub vulnerable_version_range ("< 1.2.3", ">= 1.0.0, <= 1.2.2").
in_range() {
  local v=$1 clause op bound
  IFS=',' read -ra clauses <<<"$2"
  for clause in "${clauses[@]}"; do
    read -r op bound <<<"$clause"
    bound=${bound#v}
    case "$op" in
      "<")  ver_le "$v" "$bound" && [ "$v" != "$bound" ] || return 1 ;;
      "<=") ver_le "$v" "$bound" || return 1 ;;
      ">")  ver_le "$bound" "$v" && [ "$v" != "$bound" ] || return 1 ;;
      ">=") ver_le "$bound" "$v" || return 1 ;;
      "=")  [ "$v" = "$bound" ] || return 1 ;;
      *) echo "advisory-status: cannot parse range clause '$clause'" >&2; exit 2 ;;
    esac
  done
}

# shellcheck disable=SC2016 # $id and $st are jq variables, not shell ones
rows=$(gh api --paginate "repos/$repo/security-advisories?per_page=100" --jq \
  '.[] | .ghsa_id as $id | .state as $st | .vulnerabilities[] |
   [$id, $st, (.vulnerable_version_range // ""), (.patched_versions // "")] | @tsv')
if [ -z "$rows" ]; then
  # An empty list is a token that cannot see the advisories as often as it is
  # a repository without any; either way it proves nothing.
  echo "advisory-status: the API returned no advisories for $repo" >&2
  exit 2
fi

flagged=0
printf 'release v%s\n\n' "$release"
printf '%-21s %-10s %-18s %-10s %s\n' ADVISORY STATE RANGE PATCHED NOTE
while IFS=$'\t' read -r id state range patched; do
  note=""
  if [ -n "$patched" ]; then
    if ! git rev-parse -q --verify "refs/tags/v${patched#v}" >/dev/null; then
      note="NO-TAG: patched_versions names v${patched#v}, which has no tag"
      flagged=1
    fi
  elif [ "$state" = published ] && [ -n "$range" ]; then
    if in_range "$release" "$range"; then
      note="no fixed release yet (v$release is in range)"
    else
      note="STALE: v$release is outside the range; set patched_versions if its fix is in it"
      flagged=1
    fi
  elif [ "$state" = draft ]; then
    note="draft: publishing is the owner's decision"
  fi
  printf '%-21s %-10s %-18s %-10s %s\n' "$id" "$state" "$range" "${patched:--}" "$note"
done <<<"$rows"
exit "$flagged"
