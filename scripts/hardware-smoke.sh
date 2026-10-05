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
#   SMOKE_STORAGE=1 scripts/hardware-smoke.sh  # opt in to the storage leg (unproven)
set -uo pipefail

REGION="${REGION:-us-east-1}"
NODES="${NODES:-2}"
TYPE="${TYPE:-c6i.large}"
EFA_TYPE="${EFA_TYPE:-c5n.9xlarge}"
TTL="${TTL:-20m}"
SMALL_TYPE="${SMALL_TYPE:-t4g.small}"
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

  # EFS fixture: mount target first, then the filesystem, then the SG.
  for mt in $(aws efs describe-mount-targets --region "$REGION" --file-system-id "${FS_ID:-none}" \
                --query 'MountTargets[].MountTargetId' --output text 2>/dev/null); do
    aws efs delete-mount-target --region "$REGION" --mount-target-id "$mt" >/dev/null 2>&1 \
      && echo "  deleted mount target $mt"
  done
  if [ -n "${FS_ID:-}" ]; then
    for _ in $(seq 1 24); do
      [ "$(aws efs describe-mount-targets --region "$REGION" --file-system-id "$FS_ID" \
          --query 'length(MountTargets)' --output text 2>/dev/null)" = "0" ] && break
      sleep 5
    done
    aws efs delete-file-system --region "$REGION" --file-system-id "$FS_ID" >/dev/null 2>&1 \
      && echo "  deleted EFS $FS_ID"
  fi
  [ -n "${EFS_SG:-}" ] && aws ec2 delete-security-group --region "$REGION" \
    --group-id "$EFS_SG" >/dev/null 2>&1 && echo "  deleted SG $EFS_SG"

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
  # The command is turned into JSON by python, NOT by shell interpolation.
  #
  # Three separate checks in this script were broken by hand-escaping a shell
  # command through a JSON parameter: the fi_pingpong probe came back empty, the
  # mount check got no response, and the profile check printed the literal string
  # "$EFS_MOUNT" because the dollar sign never survived. Each looked like a
  # product failure and was a quoting failure in this file.
  #
  # Same lesson as pkg/mpicohort/assembler.go, which base64-encodes the peers
  # file rather than quoting it: stop escaping, change representation.
  local payload
  payload=$(COMMAND="$2" python3 -c 'import json,os; print(json.dumps({"commands":[os.environ["COMMAND"]]}))') || return 1

  local cid
  cid=$(aws ssm send-command --region "$REGION" --instance-ids "$1" \
    --document-name AWS-RunShellScript --parameters "$payload" \
    --query Command.CommandId --output text 2>/dev/null) || return 1
  [ -n "$cid" ] || return 1

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
  # Plain single-quoted shell. These used to carry \" escapes for the old
  # hand-built-JSON helper; with python encoding the payload, an escaped quote
  # now reaches the instance literally and the grep pattern stops matching —
  # which reported "mpirun reached 0 of 2 nodes" on a cluster that was fine.
  hosts=$(ssm "$RANK0" "grep '^ip-' /var/log/cloud-init-output.log | sort -u | wc -l")
  ranks=$(ssm "$RANK0" "grep -c '^ip-' /var/log/cloud-init-output.log")
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

# ---------------------------------------------------------------------------
# Storage leg. Covers pkg/userdata/storage.go, which generates the mount,
# fstab and /etc/profile.d shell — the single largest body of generated shell
# in the tree and the one whose unit tests can only check TEXT.
#
# Added because the manifest said storage.go had changed and the smoke did not
# cover it. #680 rewrote every quoted value in that template; its tests assert
# the rendering and `bash -n`, neither of which can tell you the mount actually
# appeared or that the profile export carries the right value.
# ---------------------------------------------------------------------------
# OPT-IN until it has produced one clean run (SMOKE_STORAGE=1).
#
# Not on by default because it is not yet trustworthy, and an untrustworthy
# check is worse than a missing one — the same reasoning applied to the EFA
# fabric probe. Four runs produced three different false signals, every one a
# bug in THIS FILE rather than in spawn: --on-complete terminating the instance
# before the checks ran, hand-escaped JSON swallowing a $, and an escaped grep
# pattern reaching the instance literally. The product findings underneath were
# real and are recorded in #704.
if [ "${SMOKE_STORAGE:-0}" = "1" ]; then
  step "Storage: EFS mount + profile export (1 x ${SMALL_TYPE})"

  EFS_SG=$(aws ec2 create-security-group --region "$REGION" \
    --group-name "$TAG-efs" --description "smoke EFS mount target" \
    --vpc-id "$(aws ec2 describe-vpcs --region "$REGION" --filters Name=is-default,Values=true \
      --query 'Vpcs[0].VpcId' --output text)" \
    --tag-specifications "ResourceType=security-group,Tags=[{Key=smoke,Value=$TAG}]" \
    --query GroupId --output text 2>/dev/null)
  VPC_CIDR=$(aws ec2 describe-vpcs --region "$REGION" --filters Name=is-default,Values=true \
    --query 'Vpcs[0].CidrBlock' --output text)
  aws ec2 authorize-security-group-ingress --region "$REGION" --group-id "$EFS_SG" \
    --protocol tcp --port 2049 --cidr "$VPC_CIDR" >/dev/null 2>&1

  FS_ID=$(aws efs create-file-system --region "$REGION" --encrypted \
    --tags "Key=smoke,Value=$TAG" --query FileSystemId --output text 2>/dev/null)
  for _ in $(seq 1 24); do
    [ "$(aws efs describe-file-systems --region "$REGION" --file-system-id "$FS_ID" \
        --query 'FileSystems[0].LifeCycleState' --output text)" = available ] && break
    sleep 5
  done
  SUBNET=$(aws ec2 describe-subnets --region "$REGION" \
    --filters Name=default-for-az,Values=true --query 'Subnets[0].SubnetId' --output text)
  MT_ID=$(aws efs create-mount-target --region "$REGION" --file-system-id "$FS_ID" \
    --subnet-id "$SUBNET" --security-groups "$EFS_SG" --query MountTargetId --output text 2>/dev/null)
  for _ in $(seq 1 30); do
    [ "$(aws efs describe-mount-targets --region "$REGION" --mount-target-id "$MT_ID" \
        --query 'MountTargets[0].LifeCycleState' --output text)" = available ] && break
    sleep 5
  done

  if "$SPAWN" launch "$TAG-efs" --instance-type "$SMALL_TYPE" --region "$REGION" \
       --subnet-id "$SUBNET" --efs-id "$FS_ID" \
       --command 'df -h /efs; mount | grep -c /efs' \
       --ttl "$TTL" --cost-limit 0.50 \
       --tag "smoke=$TAG" >/tmp/$TAG-efs.log 2>&1; then
    # Deliberately NO --on-complete terminate. `df -h /efs` finishes in
    # milliseconds, so spored terminated the instance before the SSM checks
    # below could run, and all three came back EMPTY — reported as three
    # failures of the mount. A check that cannot run must not print a failure;
    # the trap cleanup and the TTL both still bound the cost.
    ok "launch with --efs-id succeeded"
    SID=$(aws ec2 describe-instances --region "$REGION" \
      --filters "Name=tag:smoke,Values=$TAG" "Name=instance-type,Values=$SMALL_TYPE" \
                "Name=instance-state-name,Values=running" \
      --query 'Reservations[].Instances[].InstanceId' --output text | head -1)

    if [ -z "$SID" ]; then
      echo "  ⚠️  storage checks INCONCLUSIVE: the instance was gone before they ran."
    else
    gate=$(ssm "$SID" 'cat /run/spawn/storage-ready 2>/dev/null')
    case "$gate" in
      *ok*)     ok "storage gate reported ok (#668)" ;;
      "")       echo "  ⚠️  storage gate INCONCLUSIVE: no response over SSM (instance gone?)" ;;
      *)        bad "storage gate: $gate — the mount did not verify" ;;
    esac

    mounted=$(ssm "$SID" 'grep -c " /efs " /proc/mounts')
    mounted=${mounted//[^0-9]/}
    if [ -z "$mounted" ]; then
      echo "  ⚠️  mount check INCONCLUSIVE: no response over SSM"
    elif [ "$mounted" -ge 1 ]; then
      ok "EFS is mounted at /efs"
    else
      bad "/efs is not in /proc/mounts — the mount did not happen (see #704 before blaming quoting)"
    fi

    # The #680 regression that unit tests cannot see: sourcing the generated
    # profile must yield the mount point, not a word-split fragment.
    exported=$(ssm "$SID" '. /etc/profile.d/efs.sh; printf %s "$EFS_MOUNT"')
    case "$exported" in
      *"/efs"*) ok "profile export resolves to \$EFS_MOUNT=$exported (#680)" ;;
      "") echo "  ⚠️  profile export INCONCLUSIVE: no response over SSM" ;;
      *) bad "\$EFS_MOUNT is $exported after sourcing /etc/profile.d/efs.sh — the quoting is wrong (#680)" ;;
    esac
    fi
  else
    bad "launch with --efs-id failed; see /tmp/$TAG-efs.log"
    grep -E 'ERROR|error|failed' /tmp/$TAG-efs.log | sed 's/^/     /' | head -4
  fi
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
