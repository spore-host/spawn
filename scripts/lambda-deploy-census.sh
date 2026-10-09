#!/usr/bin/env bash
#
# Fail if any spore.host-operated Lambda deploy path omits a property every one of
# them must set: a readable version (#654) or a log retention (#653). With
# --deployed, also report live versions against this working tree.
#
# Why this exists (spawn#654). Nothing synchronises the CLI with the control
# plane: `spawn` upgrades when a user upgrades it, and a Lambda upgrades only
# when a human runs its deploy. So version skew is the NORMAL state, which is
# defensible — the two are independently deployed — but it was INVISIBLE.
# `spawn-ttl-reaper-production` sat untouched from 2026-07-31 to 2026-10-04 while
# the CLI went from ~v0.9x to v0.116.0, missing three merged fixes, and nothing
# anywhere said so.
#
# Answering "is the deployed reaper current?" used to mean comparing the
# function's LastModified against `git log -- lambda/<name>/`. That is
# archaeology, it was done by hand four times in one session, and it is wrong as
# often as it is right: a redeploy with no source change moves LastModified, and
# a source change with no redeploy does not.
#
# Two halves, mirroring scripts/lambda-runtime-census.sh and verify-pins.sh:
#
#   offline   every deploy mechanism in this repo must stamp spawn:version. No
#             credentials, so it gates every PR — this is the half that stops a
#             NEW lambda arriving unstamped, which is the structural fix.
#   deployed  reads the tag off each live function and compares it to this tree.
#             Operator-facing: these functions live in spore.host's infra
#             account, so a user's CLI cannot see them (which is why the
#             user-facing half of #654 is `spawn reaper status`, for the
#             SELF-HOSTED reaper in their own account — a different question).
#
# Usage:
#   lambda-version-census.sh                       # offline only
#   AWS_PROFILE=spore-host-infra lambda-version-census.sh --deployed
set -uo pipefail

REGION="${REGION:-us-east-1}"
fail=0
note() { printf '  %s\n' "$1" >&2; }

# ---------------------------------------------------------------------------
# Offline: every deploy mechanism must stamp spawn:version.
#
# TWO forms, not one, and that is the whole reason this half is not a single
# grep. A SAM template declares the tag in YAML; a shell deploy sets it with
# `aws lambda tag-resource`. Checking only templates is what let the
# sweep-orchestrator and scheduler-handler go unstamped — they are deployed by
# scripts, and no YAML key exists to find.
# ---------------------------------------------------------------------------
echo "=== offline: deploy mechanisms must stamp spawn:version and set log retention"

for t in lambda/*/template.yaml; do
  [ -e "$t" ] || continue
  name=$(basename "$(dirname "$t")")
  # The ASSIGNMENT, not the string. A bare `grep spawn:version` also matches the
  # parameter's own Description, which mentions the tag by name — so reverting
  # the real tag left the gate green. Verified by reverting it (see below).
  grep -q "AWS::Serverless::Function" "$t" || { note "✅ $name (SAM template, no function)"; continue; }
  ok=1
  if ! grep -qE "^[[:space:]]*spawn:version:[[:space:]]*!Ref[[:space:]]+Version" "$t"; then
    note "❌ $t declares a function but never stamps spawn:version"
    fail=1; ok=0
  fi
  # A declared LogGroup with a retention is the only thing that stops Lambda
  # auto-creating an immortal one on first invocation (#653).
  if ! grep -qE "^[[:space:]]*RetentionInDays:" "$t"; then
    note "❌ $t declares a function but no log group retention"
    fail=1; ok=0
  fi
  [ "$ok" = "1" ] && note "✅ $name (SAM template)"
done

for s in scripts/deploy-*.sh; do
  [ -e "$s" ] || continue
  # Only scripts that actually create or update a function; the setup-* helpers
  # that make roles and buckets have no function to stamp.
  grep -qE "aws lambda (create-function|update-function-code)" "$s" || continue
  # Must appear inside a --tags value, not merely somewhere in the file: these
  # scripts also echo the stamp and explain it in comments, both of which a bare
  # substring match accepts. That exact revert passed before this was tightened.
  ok=1
  if ! grep -qE -- "--tags.*spawn:version=" "$s"; then
    note "❌ $s deploys a function but never stamps spawn:version"
    fail=1; ok=0
  fi
  # The INVOCATION, not the string: the comment above each call explains what
  # put-retention-policy does, so a bare substring match passes on a script that
  # only talks about it. That exact revert passed before this was tightened — the
  # third time in one session a gate matched its own explanatory prose.
  if ! grep -qE "^[[:space:]]*aws logs put-retention-policy" "$s"; then
    note "❌ $s deploys a function but never sets log retention"
    fail=1; ok=0
  fi
  [ "$ok" = "1" ] && note "✅ $(basename "$s") (shell deploy)"
done

if [ "$fail" -ne 0 ]; then
  echo
  echo "A deployed Lambda with no version tag cannot be checked for skew, and skew" >&2
  echo "is the normal state here — the CLI and the control plane are deployed" >&2
  echo "independently (#654). Stamp it: a SAM template takes a Version parameter" >&2
  echo "and a spawn:version tag; a shell deploy calls 'aws lambda tag-resource'." >&2
  echo "For retention: a SAM template declares an AWS::Logs::LogGroup with" >&2
  echo "RetentionInDays; a shell deploy calls 'aws logs put-retention-policy'." >&2
  exit 1
fi

[ "${1:-}" = "--deployed" ] || { echo; echo "offline half passed. Pass --deployed to compare live versions."; exit 0; }

# ---------------------------------------------------------------------------
# Deployed: what is live, versus this tree.
# ---------------------------------------------------------------------------
echo
echo "=== deployed: live spawn:version vs this working tree"

# The same derivation every deploy path uses, so a match here means a match.
tree_version=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)
echo "  this tree: $tree_version"
case "$tree_version" in
  *-dirty)
    note "(this tree has uncommitted changes, so a mismatch below may just be local edits)"
    ;;
esac
echo

# ENUMERATE, do not hardcode. Two reasons, both found the hard way on the first
# run of this script:
#
#   1. The first version listed five function names it expected. The account has
#      twelve, several last deployed in February — so a hardcoded list reports a
#      clean bill of health for everything it forgot.
#   2. One function is called `scheduler-handler`, with NO spawn- prefix
#      (scripts/deploy-scheduler-handler.sh: SPAWN_LAMBDA_NAME:-scheduler-handler).
#      So even enumerating `spawn-*` would miss it. This is the same lesson
#      scripts/lambda-runtime-census.sh records about github-oauth-bridge: a
#      filter built from what you remember cannot find what you forgot.
#
# This is spore.host's own infra account, so every function in it is ours to
# account for. Listing all of them means nothing hides.
aws lambda list-functions --region "$REGION" \
  --query 'Functions[].[FunctionName,LastModified]' --output text 2>/dev/null |
  sort | while IFS=$'\t' read -r fn modified; do
    [ -n "$fn" ] || continue
    arn=$(aws lambda get-function-configuration --function-name "$fn" \
      --region "$REGION" --query FunctionArn --output text 2>/dev/null)
    v=$(aws lambda list-tags --resource "$arn" --region "$REGION" \
      --query 'Tags."spawn:version"' --output text 2>/dev/null)

    if [ -z "$v" ] || [ "$v" = "None" ]; then
      # Deliberately NOT a failure. An unstamped function predates this check,
      # and the offline half guarantees its NEXT deploy stamps it. Calling it a
      # mismatch would be a guess — the same distinction cmd/reaper_skew.go
      # draws between "unknown" and "different".
      note "?  $fn: no spawn:version tag (last deployed $modified)"
    elif [ "$v" = "$tree_version" ]; then
      note "OK $fn: $v"
    else
      note "!  $fn: deployed $v, tree $tree_version (last deployed $modified)"
    fi
  done

echo
echo "Skew is not an error — these are deployed independently of the CLI (#654)."
echo "It is reported so that it is a fact someone knows, rather than one they"
echo "would have to go and reconstruct."
