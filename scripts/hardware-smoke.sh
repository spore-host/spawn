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
# The MPI job-array name, and therefore the prefix of every managed security
# group and placement group the MPI leg creates. Derived ONCE: it is
# "${TAG//-/}" because --job-array-name takes no hyphens, and computing it a
# second time in the teardown is what made the cleanup and the leak check miss
# every resource they were added to catch. They filtered spawn-mpi-smoke-26176-*
# while the real groups were spawn-mpi-smoke26176-*, so both reported success
# over four orphans.
ARRAY_NAME="${TAG//-/}"

[ -x "$SPAWN" ] || { echo "no spawn binary at $SPAWN — run 'make build' (which writes bin/spawn) or set SPAWN=" >&2; exit 2; }
command -v aws >/dev/null || { echo "aws CLI required" >&2; exit 2; }

pass=0; fail=0
ok()   { echo "  ✅ $*"; pass=$((pass+1)); }
bad()  { echo "  ❌ $*"; fail=$((fail+1)); }
step() { echo; echo "=== $*"; }

# Terminate everything we created, whatever happens. This runs on error and on
# interrupt, because the one thing worse than a failed smoke is a leaked cluster.
# smoke_live_instances lists this run's instances in any NON-terminal state.
#
# shutting-down is included deliberately: it is excluded from EC2's own default
# state filter, which is the blind spot behind #736 — a terminating instance
# reported as nonexistent. Leaving it out would make "still shutting down" and
# "terminate failed and it is stuck" indistinguishable, which is the one
# distinction a leak check exists to draw.
smoke_live_instances() {
  {
    aws ec2 describe-instances --region "$REGION" \
      --filters "Name=tag:smoke,Values=$TAG" \
                "Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down" \
      --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null
    aws ec2 describe-instances --region "$REGION" \
      --filters "Name=tag:Name,Values=$TAG-sw-*" \
                "Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down" \
      --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null
  } | tr '\t' '\n' | grep '^i-' || true
}

cleanup() {
  step "Cleanup"
  local ids
  # TWO filters, not one.
  #
  # The MPI and storage legs tag their instances smoke=$TAG. The sweep leg's rows
  # cannot be tagged that way: --tag is one of the 18 flags still dropped on
  # sweeps (#697's remainder), so it never reaches a row. They are found by the
  # Name prefix spawn assigns each row instead.
  #
  # Getting this wrong would leak the sweep's instances while printing "no
  # instances left behind", which is the false-pass this check already produced
  # once for placement groups.
  ids=$(
    aws ec2 describe-instances --region "$REGION" \
      --filters "Name=tag:smoke,Values=$TAG" \
                "Name=instance-state-name,Values=pending,running,stopping,stopped" \
      --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | tr '\t' '\n'
    aws ec2 describe-instances --region "$REGION" \
      --filters "Name=tag:Name,Values=$TAG-sw-*" \
                "Name=instance-state-name,Values=pending,running,stopping,stopped" \
      --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | tr '\t' '\n'
  )
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

  # Sweep leg's own EFS fixture (#697). Separate from the storage leg's because
  # the two legs can run independently; a shared variable would leave one of them
  # leaking whichever ran second.
  for mt in $(aws efs describe-mount-targets --region "$REGION" --file-system-id "${SWEEP_FS:-none}" \
                --query 'MountTargets[].MountTargetId' --output text 2>/dev/null); do
    aws efs delete-mount-target --region "$REGION" --mount-target-id "$mt" >/dev/null 2>&1 \
      && echo "  deleted sweep mount target $mt"
  done
  if [ -n "${SWEEP_FS:-}" ]; then
    for _ in $(seq 1 24); do
      [ "$(aws efs describe-mount-targets --region "$REGION" --file-system-id "$SWEEP_FS" \
          --query 'length(MountTargets)' --output text 2>/dev/null)" = "0" ] && break
      sleep 5
    done
    aws efs delete-file-system --region "$REGION" --file-system-id "$SWEEP_FS" >/dev/null 2>&1 \
      && echo "  deleted sweep EFS $SWEEP_FS"
  fi
  [ -n "${SWEEP_SG:-}" ] && aws ec2 delete-security-group --region "$REGION" \
    --group-id "$SWEEP_SG" >/dev/null 2>&1 && echo "  deleted sweep SG $SWEEP_SG"

  # The MPI leg's own managed infrastructure. This file terminated its instances
  # and then left these behind on every run — three orphaned placement groups and
  # three security groups accumulated across one session, which `spawn orphans`
  # found once #708 made the default scope work:
  #
  #   ec2  placement-group  spawn-mpi-smoke22812-us-east-1a  empty
  #   ec2  placement-group  spawn-mpi-smoke85337-us-east-1a  empty
  #
  # A smoke that exists to catch leaks must not be a source of them. Placement
  # group deletion is retried because TerminateInstances is asynchronous and EC2
  # refuses to delete a group with members — the same race #685 fixed in spawn
  # itself, and this script has to honour it for the same reason.
  for az_pg in $(aws ec2 describe-placement-groups --region "$REGION" \
      --filters "Name=group-name,Values=spawn-mpi-${ARRAY_NAME}-*" \
      --query 'PlacementGroups[].GroupName' --output text 2>/dev/null | tr '\t' '\n'); do
    [ -n "$az_pg" ] || continue
    for _ in $(seq 1 12); do
      aws ec2 delete-placement-group --region "$REGION" --group-name "$az_pg" >/dev/null 2>&1 \
        && { echo "  deleted placement group $az_pg"; break; }
      sleep 5
    done
  done

  for mpi_sg in $(aws ec2 describe-security-groups --region "$REGION" \
      --filters "Name=group-name,Values=spawn-mpi-${ARRAY_NAME}*" \
      --query 'SecurityGroups[].GroupId' --output text 2>/dev/null | tr '\t' '\n'); do
    [ -n "$mpi_sg" ] || continue
    for _ in $(seq 1 12); do
      aws ec2 delete-security-group --region "$REGION" --group-id "$mpi_sg" >/dev/null 2>&1 \
        && { echo "  deleted SG $mpi_sg"; break; }
      sleep 5
    done
  done

  # Independent leak check: ask AWS, do not trust the terminate calls above.
  #
  # POLLS until the live set is empty rather than sleeping a guess, and includes
  # shutting-down in the filter (#752).
  #
  # The old form was `sleep 10` then one query over
  # pending,running,stopping,stopped. That filter omits shutting-down — the same
  # blind spot as #736, where EC2's default state filter made a terminating
  # instance report as nonexistent — so the sleep bought nothing: a
  # still-terminating instance was invisible with or without it, while a FAILED
  # terminate leaves state `running` and is caught instantly. Worse, "invisible"
  # and "gone" were indistinguishable, so a terminate that hung would have
  # reported no leak.
  #
  # Cost control is existential here, so the check now distinguishes
  # "still shutting down" (wait) from "stuck" (report it).
  local left=""
  local waited=0
  while :; do
    left=$(smoke_live_instances)
    [ -z "$left" ] && break
    [ "$waited" -ge 120 ] && break
    sleep 5
    waited=$((waited + 5))
  done
  left=$(printf '%s\n' "$left" | grep -c '^i-' || true)
  if [ "${left:-0}" = "0" ]; then ok "no instances left behind"; else bad "LEAK: $left instance(s) still alive — terminate by hand NOW"; fi

  # Instances are the expensive leak, but not the only one. Three placement
  # groups and three security groups accumulated over one session because this
  # check only ever asked about instances, so it reported "no instances left
  # behind" while leaving litter every run (#685/#708).
  local pg_left sg_left
  pg_left=$(aws ec2 describe-placement-groups --region "$REGION" \
    --filters "Name=group-name,Values=spawn-mpi-${ARRAY_NAME}-*" \
    --query 'length(PlacementGroups)' --output text 2>/dev/null)
  sg_left=$(aws ec2 describe-security-groups --region "$REGION" \
    --filters "Name=group-name,Values=spawn-mpi-${ARRAY_NAME}*" \
    --query 'length(SecurityGroups)' --output text 2>/dev/null)
  local efs_left
  efs_left=$(aws efs describe-file-systems --region "$REGION" \
    --query "length(FileSystems[?Name=='$TAG-sweep'])" --output text 2>/dev/null)
  if [ "${efs_left:-0}" != "0" ]; then
    bad "LEAK: the sweep leg's EFS filesystem is still present — it bills per GiB"
  fi
  if [ "${pg_left:-0}" = "0" ] && [ "${sg_left:-0}" = "0" ]; then
    ok "no placement groups or security groups left behind"
  else
    bad "LEAK: ${pg_left:-?} placement group(s) and ${sg_left:-?} security group(s) remain — 'spawn orphans --region $REGION' will list them"
  fi
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

# ssm_wait_online blocks until the SSM agent registers, so a later empty reply
# means "the file is not there" rather than "nobody was listening". `spawn
# launch` returns at state=running, which is well before the agent is up —
# asserting through a channel that is not open yet is how the storage leg
# concluded a mount had failed when it had not been attempted.
ssm_wait_online() {
  for _ in $(seq 1 60); do
    [ "$(aws ssm describe-instance-information --region "$REGION" \
        --filters "Key=InstanceIds,Values=$1" \
        --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null)" = Online ] && return 0
    sleep 5
  done
  return 1
}

# storage_wait_settled blocks until cloud-init is done, because that is when the
# bootstrap has finished trying to mount. Polling for the mount itself would be
# wrong in the other direction: it cannot distinguish "not yet" from "never", and
# a 60-second retry budget (#704) means the mount is legitimately absent for up
# to a minute after boot.
storage_wait_settled() {
  for _ in $(seq 1 60); do
    case "$(ssm "$1" 'cloud-init status 2>/dev/null | head -1')" in
      *done*|*error*|*disabled*) return 0 ;;
    esac
    sleep 5
  done
  return 1
}

step "MPI cohort: ${NODES} x ${TYPE} in ${REGION}"
if "$SPAWN" launch "$TAG-mpi" --instance-type "$TYPE" --region "$REGION" \
     --count "$NODES" --job-array-name "$ARRAY_NAME" --mpi \
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
  # POLL for mpirun's output rather than sleeping a constant (#752).
  #
  # This was `sleep 45   # let mpirun finish`. Two reasons that is a worse bet
  # now: rank 0's mpirun no longer starts at a fixed offset (it waits for every
  # peer to accept SSH first), and the wait needed grows with NODES while a
  # constant does not. The comment below records that this very assertion has
  # already mis-reported once on a healthy cluster, so making it time-dependent
  # was the wrong trade.
  #
  # Plain single-quoted shell. These used to carry \" escapes for the old
  # hand-built-JSON helper; with python encoding the payload, an escaped quote
  # now reaches the instance literally and the grep pattern stops matching —
  # which reported "mpirun reached 0 of 2 nodes" on a cluster that was fine.
  hosts=""
  ranks=""
  mpi_waited=0
  while [ "$mpi_waited" -lt 300 ]; do
    hosts=$(ssm "$RANK0" "grep '^ip-' /var/log/cloud-init-output.log | sort -u | wc -l")
    ranks=$(ssm "$RANK0" "grep -c '^ip-' /var/log/cloud-init-output.log")
    [ "${hosts//[^0-9]/}" = "$NODES" ] && break
    sleep 10
    mpi_waited=$((mpi_waited + 10))
  done
  [ "${hosts//[^0-9]/}" = "$NODES" ] \
    && echo "     (all $NODES nodes reported after ${mpi_waited}s)" \
    || echo "     (gave up after ${mpi_waited}s with ${hosts//[^0-9]/} node(s))"
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

    # ---- Prove the channel works BEFORE asserting anything through it. ----
    #
    # The fifth false signal from this file (#704's first run): the leg asserted
    # on the mount the instance had not performed yet. `spawn launch` returns at
    # state=running, which is minutes before cloud-init installs nfs-utils and
    # mounts. All three checks came back empty and the leg reported "the mount
    # did not happen" — an outright wrong conclusion, drawn from a check that had
    # not run.
    #
    # Worse, every branch below conflated EMPTY OUTPUT with NO RESPONSE, so a
    # file that merely did not exist yet was indistinguishable from an SSM
    # failure. Both of those are now impossible: reachability is established
    # once, out loud, and every probe prints a sentinel so absence is a VALUE.
    if ! ssm_wait_online "$SID"; then
      echo "  ⚠️  storage checks INCONCLUSIVE: SSM never came online for $SID in 5 min."
      echo "      Nothing is claimed about the mount — the channel to ask never opened."
    elif ! storage_wait_settled "$SID"; then
      echo "  ⚠️  storage checks INCONCLUSIVE: cloud-init had not finished in 5 min, so the"
      echo "      bootstrap may simply not have reached the mount yet."
      ssm "$SID" 'tail -40 /var/log/cloud-init-output.log' | sed 's/^/      | /'
    else

    gate=$(ssm "$SID" 'if [ -f /run/spawn/storage-ready ]; then cat /run/spawn/storage-ready; else echo ABSENT; fi')
    case "$gate" in
      *ok*)      ok "storage gate reported ok (#668)" ;;
      *ABSENT*)  bad "/run/spawn/storage-ready does not exist — the storage block never ran (#668 writes it either way)" ;;
      "")        echo "  ⚠️  storage gate INCONCLUSIVE: empty reply on a channel that just worked" ;;
      *)         bad "storage gate: $gate — the mount did not verify" ;;
    esac

    mounted=$(ssm "$SID" 'grep -c " /efs " /proc/mounts || true')
    mounted=${mounted//[^0-9]/}
    if [ -z "$mounted" ]; then
      echo "  ⚠️  mount check INCONCLUSIVE: empty reply on a channel that just worked"
    elif [ "$mounted" -ge 1 ]; then
      ok "EFS is mounted at /efs"
    else
      bad "/efs is not in /proc/mounts — the mount did not happen"
    fi

    # The #680 regression that unit tests cannot see: sourcing the generated
    # profile must yield the mount point, not a word-split fragment.
    exported=$(ssm "$SID" 'if [ -f /etc/profile.d/efs.sh ]; then . /etc/profile.d/efs.sh; printf %s "$EFS_MOUNT"; else printf ABSENT; fi')
    case "$exported" in
      *"/efs"*)  ok "profile export resolves to \$EFS_MOUNT=$exported (#680)" ;;
      *ABSENT*)  bad "/etc/profile.d/efs.sh does not exist — the storage block never ran" ;;
      "")        echo "  ⚠️  profile export INCONCLUSIVE: empty reply on a channel that just worked" ;;
      *)         bad "\$EFS_MOUNT is $exported after sourcing /etc/profile.d/efs.sh — the quoting is wrong (#680)" ;;
    esac

    # Whatever the verdict, show what the mount actually DID. This is the only
    # direct evidence for #704: a transient DNS failure that the retry then
    # recovers from prints "EFS mount attempt N/6 failed", and a success on the
    # first try prints nothing — so the absence of those lines is informative too.
    echo "  --- EFS lines from cloud-init-output.log ---"
    ssm "$SID" 'grep -iE "efs|nfs" /var/log/cloud-init-output.log | tail -25 || echo "(no EFS lines)"' \
      | sed 's/^/      | /'

    fi
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

# ---------------------------------------------------------------------------
# Sweep: does a parameter sweep honour CLI flags? (spawn#697)
#
# OPT-IN (SMOKE_SWEEP=1), and it exists because of a specific transition risk
# rather than a standing one.
#
# Until #697 the sweep path merged param rows onto an EMPTY config, so every CLI
# flag was dropped: 83 of 127, including --efs-id and all eleven --fsx-*. A sweep
# could not mount anything. The fix starts each row from the real CLI config, so
# a sweep with --efs-id now ACTUALLY ATTEMPTS AN EFS MOUNT ON EVERY ROW — code
# that has never run on this path before.
#
# That is what needs an instance to settle. `make smoke-needed` does not flag the
# sweep files, and it is right not to: the config construction is pure data and
# fully unit-tested. What a unit test cannot tell you is whether the mount
# actually happens on each of N concurrently-launched rows.
# ---------------------------------------------------------------------------
if [ "${SMOKE_SWEEP:-0}" = "1" ]; then
  step "Sweep: 2 rows x ${SMALL_TYPE} honouring --efs-id (#697)"

  SWEEP_SG=$(aws ec2 create-security-group --region "$REGION" \
    --group-name "$TAG-sweep-efs" --description "smoke sweep EFS mount target" \
    --vpc-id "$(aws ec2 describe-vpcs --region "$REGION" --filters Name=is-default,Values=true \
      --query 'Vpcs[0].VpcId' --output text)" --query GroupId --output text)
  SWEEP_VPC_CIDR=$(aws ec2 describe-vpcs --region "$REGION" --filters Name=is-default,Values=true \
    --query 'Vpcs[0].CidrBlock' --output text)
  aws ec2 authorize-security-group-ingress --region "$REGION" --group-id "$SWEEP_SG" \
    --protocol tcp --port 2049 --cidr "$SWEEP_VPC_CIDR" >/dev/null 2>&1

  SWEEP_FS=$(aws efs create-file-system --region "$REGION" --encrypted \
    --tags "Key=Name,Value=$TAG-sweep" --query FileSystemId --output text)
  for _ in $(seq 1 30); do
    [ "$(aws efs describe-file-systems --region "$REGION" --file-system-id "$SWEEP_FS" \
        --query 'FileSystems[0].LifeCycleState' --output text)" = available ] && break
    sleep 5
  done
  SWEEP_SUBNET=$(aws ec2 describe-subnets --region "$REGION" \
    --filters Name=default-for-az,Values=true --query 'Subnets[0].SubnetId' --output text)
  SWEEP_MT=$(aws efs create-mount-target --region "$REGION" --file-system-id "$SWEEP_FS" \
    --subnet-id "$SWEEP_SUBNET" --security-groups "$SWEEP_SG" --query MountTargetId --output text 2>/dev/null)
  for _ in $(seq 1 30); do
    [ "$(aws efs describe-mount-targets --region "$REGION" --mount-target-id "$SWEEP_MT" \
        --query 'MountTargets[0].LifeCycleState' --output text)" = available ] && break
    sleep 5
  done

  # Two rows differing only in a parameter, so any difference between them is
  # the sweep machinery rather than the workload.
  cat > /tmp/$TAG-sweep.json <<'SWEEPJSON'
{
  "params": [
    {"trial": "a"},
    {"trial": "b"}
  ]
}
SWEEPJSON

  if "$SPAWN" launch "$TAG-sw" --instance-type "$SMALL_TYPE" --region "$REGION" \
       --param-file /tmp/$TAG-sweep.json --subnet-id "$SWEEP_SUBNET" \
       --efs-id "$SWEEP_FS" --command 'mount | grep -c " /efs "; sleep 900' \
       --ttl "$TTL" --cost-limit 0.50 \
       >/tmp/$TAG-sweep.log 2>&1; then
    ok "sweep launched with --efs-id (previously dropped entirely)"

    # Find the rows by NAME, not by --tag.
    #
    # --tag is one of the 18 flags still dropped on sweeps (#697's remainder:
    # applied imperatively in launchSingleInstance, which the sweep dispatch
    # returns before reaching). So the first version of this leg filtered on a
    # tag the rows never received and reported "found 0" while two rows were
    # running — the leg's own discovery key was a flag it was testing for absence.
    #
    # The command also now ends in `sleep 900`: `mount | grep -c` finishes in
    # milliseconds and the rows self-terminated before these checks could run,
    # which is the same trap the storage leg above already documents.
    SWEEP_IDS=$(aws ec2 describe-instances --region "$REGION" \
      --filters "Name=tag:Name,Values=$TAG-sw-*" \
                "Name=instance-state-name,Values=pending,running" \
      --query 'Reservations[].Instances[].InstanceId' --output text | tr '\t' '\n' | head -2)
    COUNT=$(printf '%s\n' "$SWEEP_IDS" | grep -c '^i-' || true)
    if [ "$COUNT" -lt 2 ]; then
      echo "  ⚠️  sweep INCONCLUSIVE: expected 2 running rows, found $COUNT"
    else
      ok "both sweep rows are running"
      MOUNTED=0
      for SID in $SWEEP_IDS; do
        ssm_wait_online "$SID" || continue
        storage_wait_settled "$SID" || continue
        N=$(ssm "$SID" 'grep -c " /efs " /proc/mounts || true')
        N=${N//[^0-9]/}
        [ "${N:-0}" -ge 1 ] && MOUNTED=$((MOUNTED + 1))
      done
      if [ "$MOUNTED" -eq 2 ]; then
        ok "EFS mounted on BOTH rows — #697's flag drop is fixed on hardware"
      else
        bad "EFS mounted on only $MOUNTED of 2 rows; --efs-id reaches the config but not the instance"
      fi
    fi
  else
    bad "sweep launch failed; see /tmp/$TAG-sweep.log"
    grep -E 'ERROR|error|failed' /tmp/$TAG-sweep.log | sed 's/^/     /' | head -4
  fi
fi
