#!/bin/bash
# Verify a DETACHED parameter sweep on real hardware: that its rows launch at
# all (#749), and that they carry the lifecycle tags which make them reapable
# (#725).
#
# Detached is the DEFAULT for parameter sweeps — launchParameterSweep
# auto-enables it — so this exercises the common path, not an opt-in one. The
# rows are launched by the spawn-sweep-orchestrator Lambda from a handful of
# param keys; it never sees a LaunchConfig, which is the root of both issues.
#
# PASSING as of 2026-10-08, 8/8, against spawn v0.126.0 and the orchestrator
# Lambda deployed the same day. This is the first run in which #725's assertions
# were observable at all.
#
# It used to fail, correctly, on #749: the CLI did not seed `ami` into the params
# the Lambda reads, so every RunInstances was rejected with `MissingParameter:
# The request must contain the parameter ImageId` — and the Lambda then logged
# "All instances launched and completed" while the CLI reported success. Zero
# instances were created, so the tag assertions below could not run.
#
# Two things have to be true at once for this test to mean anything, and only one
# of them is in the repo: the CLI must seed the params (#751, shipped in
# v0.126.0) AND the deployed orchestrator must carry the tag fix (#726) — a
# Lambda does not upgrade with the CLI, which is #654. If this test regresses,
# check the deployed function's LastModified against `git log -- lambda/sweep-orchestrator/`
# before looking for a code bug.
#
# Cost safety: the smallest ARM type, a 20-minute TTL, an explicit terminate, and
# an INDEPENDENT leak-check that re-queries rather than trusting the terminate.
#
# Usage:
#   AWS_PROFILE=spore-host-dev scripts/hardware-test-detached-sweep.sh
set -uo pipefail

REGION="${REGION:-us-east-1}"
TYPE="${TYPE:-t4g.small}"
TTL="${TTL:-20m}"
# The account the rows land in, and the account the orchestrator Lambda runs in.
# They are DIFFERENT, which this script got wrong first time: it tailed the
# Lambda's log group with the dev profile and got ResourceNotFoundException,
# losing the one diagnostic that explained the failure.
TARGET_ACCOUNT="${TARGET_ACCOUNT:-435415984226}"
INFRA_PROFILE="${INFRA_PROFILE:-spore-host-infra}"
REAPER_ROLE="${REAPER_ROLE:-arn:aws:iam::${TARGET_ACCOUNT}:role/spawn-ttl-reaper-ec2}"

TAG="dsweep-$(date +%s)"
SPAWN="${SPAWN:-./bin/spawn}"
SWEEP_ID=""
PASS=0; FAIL=0
ok()  { echo "  ✅ $*"; PASS=$((PASS + 1)); }
bad() { echo "  ❌ $*"; FAIL=$((FAIL + 1)); }

# rows lists this sweep's instances BY SWEEP ID, never by Name.
#
# The orchestrator names rows "<SweepName>-<index>", and SweepName is derived
# rather than being the name passed to `spawn launch` — so a Name filter built
# from the CLI argument matches nothing. The first version of this script did
# exactly that, which would have made the CLEANUP below miss any row that did
# launch. spawn:sweep-id is written by the orchestrator itself and is the only
# reliable handle. (The hardware smoke's sweep leg has the same lesson about
# --tag; this is the same trap one layer along.)
rows() {
  local states="$1"
  [ -z "$SWEEP_ID" ] && return 0
  aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:spawn:sweep-id,Values=$SWEEP_ID" \
              "Name=instance-state-name,Values=$states" \
    --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null |
    tr '\t' '\n' | grep '^i-' || true
}

cleanup() {
  echo
  echo "=== Cleanup"
  local ids
  ids=$(rows "pending,running,stopping,stopped")
  if [ -n "$ids" ]; then
    # shellcheck disable=SC2086
    aws ec2 terminate-instances --region "$REGION" --instance-ids $ids \
      --query 'TerminatingInstances[].InstanceId' --output text 2>/dev/null |
      tr '\t' '\n' | sed 's/^/  terminated /'
    # Wait for them to leave the live states rather than sleeping a guess, so the
    # check below distinguishes "still shutting down" from "stuck".
    for _ in $(seq 1 24); do
      [ "$(rows "pending,running,stopping,stopped,shutting-down" | grep -c '^i-' || true)" = "0" ] && break
      sleep 5
    done
  else
    echo "  (nothing to terminate)"
  fi
  # Independent: re-query rather than trusting the call above.
  #
  # shutting-down is INCLUDED. It is excluded from EC2's own default state filter
  # — the blind spot behind #736 — so leaving it out here would report "nothing
  # left behind" for an instance that is merely mid-termination, and identically
  # for one whose terminate FAILED and left it stuck.
  #
  # Counted with grep -c on instance IDs rather than a JMESPath length().
  # `length(Reservations[].Instances[?...])` counts RESERVATIONS, not instances:
  # the filter yields one list per reservation, so two reservations holding zero
  # live instances answers "2". That form gave a false leak alarm on this very
  # test and is not usable for a cost check.
  local left
  left=$(rows "pending,running,stopping,stopped,shutting-down" | grep -c '^i-' || true)
  if [ "${left:-0}" = "0" ]; then
    echo "  ✅ no instances left behind"
  else
    echo "  ❌ LEAK: ${left} instance(s) still present — terminate them by hand"
  fi
}
trap cleanup EXIT

echo "=== Building"
make build >/dev/null 2>&1 || { echo "build failed"; exit 1; }

cat > "/tmp/$TAG.json" <<'JSON'
{"params": [{"trial": "a"}, {"trial": "b"}]}
JSON

echo
echo "=== Launching a 2-row DETACHED sweep ($TYPE, ttl=$TTL)"
if ! "$SPAWN" launch "$TAG" --instance-type "$TYPE" --region "$REGION" \
     --param-file "/tmp/$TAG.json" --command 'sleep 1200' \
     --ttl "$TTL" --cost-limit 0.50 >"/tmp/$TAG.log" 2>&1; then
  bad "sweep dispatch failed"
  grep -iE 'error|failed' "/tmp/$TAG.log" | head -5 | sed 's/^/     /'
  exit 1
fi
SWEEP_ID=$(grep -oE 'sweep-[0-9]{8}-[0-9]{6}' "/tmp/$TAG.log" | head -1)
if [ -z "$SWEEP_ID" ]; then
  bad "could not parse a sweep id from the launch output"
  exit 1
fi
ok "sweep dispatched: $SWEEP_ID"

# POLL. The CLI returns as soon as the Lambda is invoked, so no instance exists
# yet and an immediate check is meaningless. Note this polling is NOT why an
# earlier run of this check reported zero rows — that was #749, a real total
# failure, which the absence of polling had merely made easy to misread.
echo
echo "=== Waiting for the orchestrator to launch the rows (up to 5 min)"
ids=""
for _ in $(seq 1 30); do
  ids=$(rows "pending,running")
  [ "$(printf '%s\n' "$ids" | grep -c '^i-' || true)" -ge 2 ] && break
  sleep 10
done
n=$(printf '%s\n' "$ids" | grep -c '^i-' || true)
if [ "${n:-0}" -lt 2 ]; then
  bad "only ${n} row(s) appeared after 5 min — expected 2"
  echo "     orchestrator log (needs the INFRA profile — the Lambda is not in the target account):"
  AWS_PROFILE="$INFRA_PROFILE" aws logs tail /aws/lambda/spawn-sweep-orchestrator \
    --region "$REGION" --since 7m --format short 2>&1 |
    grep -iE "launch|fail|error|complete" | tail -8 | sed 's/^/       /'
  echo
  echo "=== Result: $PASS passed, $FAIL failed"
  exit 1
fi
ok "both rows launched by the orchestrator"

echo
echo "=== #725: the lifecycle tags that make a row reapable"
for id in $ids; do
  echo "  --- $id"
  tags=$(aws ec2 describe-tags --region "$REGION" \
    --filters "Name=resource-id,Values=$id" \
    --query 'Tags[].[Key,Value]' --output text 2>/dev/null)
  printf '%s\n' "$tags" | sed 's/^/       /'

  managed=$(printf '%s\n' "$tags" | awk '$1=="spawn:managed"{print $2}')
  deadline=$(printf '%s\n' "$tags" | awk '$1=="spawn:ttl-deadline"{print $2}')

  if [ "$managed" = "true" ]; then
    ok "$id: spawn:managed=true — the rail the reaper's terminate is conditioned on"
  else
    bad "$id: spawn:managed=${managed:-<absent>} — the reaper CANNOT act on this row"
  fi
  if [ -n "$deadline" ]; then
    ok "$id: spawn:ttl-deadline=$deadline"
  else
    bad "$id: no spawn:ttl-deadline — only the reaper's max-age ceiling bounds it"
  fi

  # The tag is only worth having if it actually unlocks the terminate. Simulated
  # rather than performed: the point is the permission, and the cleanup below
  # does the real terminating.
  d=$(aws iam simulate-principal-policy --policy-source-arn "$REAPER_ROLE" \
    --action-names ec2:TerminateInstances \
    --resource-arns "arn:aws:ec2:${REGION}:${TARGET_ACCOUNT}:instance/${id}" \
    --context-entries "ContextKeyName=ec2:ResourceTag/spawn:managed,ContextKeyType=string,ContextKeyValues=${managed:-false}" \
    --query 'EvaluationResults[0].EvalDecision' --output text 2>/dev/null)
  if [ "$d" = "allowed" ]; then
    ok "$id: the reaper is permitted to terminate it"
  else
    bad "$id: reaper ec2:TerminateInstances = $d"
  fi
done

echo
echo "=== Result: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
