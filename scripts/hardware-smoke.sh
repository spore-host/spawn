#!/usr/bin/env bash
#
# Minimal real-AWS smoke for the paths unit tests cannot reach.
#
# This exists because v0.118.0 shipped eleven fixes of which four were findable
# ONLY on hardware, while the unit suite stayed green throughout — and three
# pre-existing tests actively asserted the broken behaviour as correct. The
# verification that found them was assembled by hand; this is that work written
# down so it costs ten minutes instead of an afternoon.
#
# Cost: roughly $0.05 at the default 2-node width. Every instance gets a TTL, is
# terminated explicitly, and is leak-checked afterwards — the leak check is not
# optional, because a drain that silently terminated nothing is one of the bugs
# this is here to catch (#683).
#
# Usage:
#   AWS_PROFILE=spore-host-dev scripts/hardware-smoke.sh
#   NODES=4 scripts/hardware-smoke.sh          # wider MPI check
#   SKIP_EFA=1 scripts/hardware-smoke.sh       # skip the EFA leg (quota/cost)
set -uo pipefail

REGION="${REGION:-us-east-1}"
NODES="${NODES:-2}"
TYPE="${TYPE:-c6i.large}"
EFA_TYPE="${EFA_TYPE:-c5n.9xlarge}"
TTL="${TTL:-20m}"
SPAWN="${SPAWN:-bin/spawn}"   # `make build` writes here, not ./spawn
TAG="smoke-$$"

[ -x "$SPAWN" ] || { echo "no spawn binary at $SPAWN — run 'make build' (which writes bin/spawn) or set SPAWN=" >&2; exit 2; }
command -v aws >/dev/null || { echo "aws CLI required" >&2; exit 2; }

pass=0; fail=0
ok()   { echo "  ✅ $*"; pass=$((pass+1)); }
bad()  { echo "  ❌ $*"; fail=$((fail+1)); }
step() { echo; echo "=== $*"; }

# Terminate everything we created, whatever happens. This runs on error and on
# interrupt, because the one thing worse than a failed smoke is a leaked cluster.
cleanup() {
  step "Cleanup"
  local ids
  ids=$(aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:smoke,Values=$TAG" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | tr '\t' '\n')
  for id in $ids; do
    [ -n "$id" ] && aws ec2 terminate-instances --region "$REGION" --instance-ids "$id" >/dev/null 2>&1 \
      && echo "  terminated $id"
  done

  # Independent leak check: ask AWS, do not trust the terminate calls above.
  sleep 10
  local left
  left=$(aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:smoke,Values=$TAG" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'length(Reservations[].Instances[])' --output text 2>/dev/null)
  if [ "${left:-0}" = "0" ]; then ok "no instances left behind"; else bad "LEAK: $left instance(s) still alive — terminate by hand NOW"; fi
}
trap cleanup EXIT INT TERM

ssm() { # $1=instance  $2=command  -> stdout
  local cid
  cid=$(aws ssm send-command --region "$REGION" --instance-ids "$1" \
    --document-name AWS-RunShellScript --parameters "commands=[\"$2\"]" \
    --query Command.CommandId --output text 2>/dev/null) || return 1
  for _ in $(seq 1 30); do
    local st
    st=$(aws ssm get-command-invocation --region "$REGION" --command-id "$cid" \
      --instance-id "$1" --query Status --output text 2>/dev/null)
    case "$st" in Success|Failed|TimedOut|Cancelled) break ;; esac
    sleep 5
  done
  aws ssm get-command-invocation --region "$REGION" --command-id "$cid" \
    --instance-id "$1" --query StandardOutputContent --output text 2>/dev/null
}

step "MPI cohort: ${NODES} x ${TYPE} in ${REGION}"
if "$SPAWN" launch "$TAG-mpi" --instance-type "$TYPE" --region "$REGION" \
     --count "$NODES" --job-array-name "${TAG//-/}" --mpi \
     --mpi-command "hostname --short" --ttl "$TTL" --cost-limit 1.00 \
     --tag "smoke=$TAG" >/tmp/$TAG-mpi.log 2>&1; then
  ok "cohort launched and assembled (#684: --mpi works end to end)"
else
  bad "cohort failed; see /tmp/$TAG-mpi.log"
  grep -E 'MPI assembly failed|members failed|drain:' /tmp/$TAG-mpi.log | sed 's/^/     /' | head -5
fi

RANK0=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:smoke,Values=$TAG" "Name=tag:spawn:job-array-index,Values=0" \
            "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | head -1)

if [ -n "$RANK0" ]; then
  sleep 45   # let mpirun finish
  hosts=$(ssm "$RANK0" 'grep \"^ip-\" /var/log/cloud-init-output.log | sort -u | wc -l')
  ranks=$(ssm "$RANK0" 'grep -c \"^ip-\" /var/log/cloud-init-output.log')
  [ "${hosts//[^0-9]/}" = "$NODES" ] \
    && ok "mpirun spread across all $NODES nodes ($ranks ranks)" \
    || bad "mpirun reached ${hosts//[^0-9]/} of $NODES nodes — the hostfile or the cluster SSH key is wrong (#684)"

  ci=$(ssm "$RANK0" 'cloud-init status')
  case "$ci" in *done*) ok "cloud-init completed (no aborted scripts-user module)" ;;
                *) bad "cloud-init reports: $ci — a user-data step failed (#684 shape)" ;; esac

  cmdfile=$(ssm "$RANK0" 'cat /etc/spawn/mpi-command')
  case "$cmdfile" in *"hostname --short"*) ok "--mpi-command preserved its arguments (#660)" ;;
                     *) bad "--mpi-command mangled: $cmdfile" ;; esac
fi

if [ "${SKIP_EFA:-0}" != "1" ]; then
  step "EFA fabric: 2 x ${EFA_TYPE} (spot)"
  if "$SPAWN" launch "$TAG-efa" --instance-type "$EFA_TYPE" --region "$REGION" \
       --count 2 --job-array-name "${TAG//-/}efa" --mpi --efa --spot \
       --mpi-command "hostname --short" --ttl "$TTL" --cost-limit 3.00 \
       --tag "smoke=$TAG" >/tmp/$TAG-efa.log 2>&1; then
    ok "EFA cohort launched"
    E0=$(aws ec2 describe-instances --region "$REGION" --filters "Name=tag:smoke,Values=$TAG" \
          "Name=instance-type,Values=$EFA_TYPE" "Name=tag:spawn:job-array-index,Values=0" \
          --query 'Reservations[].Instances[].InstanceId' --output text | head -1)
    E1=$(aws ec2 describe-instances --region "$REGION" --filters "Name=tag:smoke,Values=$TAG" \
          "Name=instance-type,Values=$EFA_TYPE" "Name=tag:spawn:job-array-index,Values=1" \
          --query 'Reservations[].Instances[].InstanceId' --output text | head -1)
    IP0=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$E0" \
          --query 'Reservations[].Instances[].PrivateIpAddress' --output text)
    # fi_pingpong uses the EFA provider EXCLUSIVELY — no TCP fallback — so a pass
    # here is proof that SRD traffic crosses the security group (#659).
    # Absolute paths, no PATH juggling. The nested-quoting version of this
    # ($PATH escaped through a JSON parameter) came back empty and reported a
    # false failure — and AWS's own EFA docs use the absolute path anyway, which
    # is what #693 pointed out.
    FIPP=/opt/amazon/efa/bin/fi_pingpong
    ssm "$E0" "nohup $FIPP -p efa > /tmp/pp.log 2>&1 & sleep 2; echo started" >/dev/null
    pp=$(ssm "$E1" "timeout 60 $FIPP -p efa $IP0 2>&1 | tail -6")

    # Three outcomes, not two. A fabric check that cannot RUN is inconclusive,
    # not a failure — reporting ❌ for a broken probe trains people to ignore
    # the smoke, which is worse than having no check.
    case "$pp" in
      *MB/sec*)
        ok "EFA SRD passes traffic through the managed SG (#659)"
        echo "$pp" | sed 's/^/     /' ;;
      "")
        echo "  ⚠️  EFA fabric check INCONCLUSIVE: no output from $FIPP."
        echo "     The cohort enrolled (so #693's probe fix holds); the fabric itself is unverified."
        echo "     Check by hand: $FIPP -p efa on rank 0, then $FIPP -p efa <rank0-ip> on rank 1." ;;
      *)
        bad "fi_pingpong ran but did not report throughput: $pp" ;;
    esac
  else
    bad "EFA cohort failed; see /tmp/$TAG-efa.log"
  fi
fi

step "Result"
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
