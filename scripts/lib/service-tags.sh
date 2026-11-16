# shellcheck shell=bash
#
# The spore-host:* service tags, for every shell deploy path in this repo
# (spore-host/spore-host#750). Sourced, never run.
#
# Why. The infra account's service resources mostly carry spawn:managed=true,
# which is also the tag spawn puts on workload litter. A litter classifier could
# not tell the two apart and marked 12 live resources as litter. So every service
# resource also carries:
#
#   spore-host:role=service
#   spore-host:component=<id>       one of SPORE_SERVICE_COMPONENTS below
#   spore-host:deployed-by=<path>   the repo-relative path of the deploying script
#
# spawn:managed stays: the CI role's tag allow-list and other code rely on it.
#
# This file is the one place the shell scripts get the keys from. The Go side
# has them in pkg/servicetags (from libs/tags) and in
# scripts/deploylambdas/servicetags.go; pkg/servicetags' TestServiceTagKeysAgree
# fails if the three disagree.
#
# Usage, BEFORE any `cd` (the path is relative to the calling script):
#
#   SPORE_SERVICE_COMPONENT=dashboard-api
#   SPORE_SERVICE_DEPLOYED_BY=lambda/dashboard-api/deploy.sh
#   # shellcheck source=scripts/lib/service-tags.sh   (relative to the repo root)
#   . "$(dirname "$0")/../../scripts/lib/service-tags.sh"
#
# and then, by the shape each AWS CLI command takes:
#
#   --tags "spawn:managed=true,${SPORE_SERVICE_TAGS_MAP}"           lambda, logs, sqs, apigatewayv2
#   --tags Key=spawn:managed,Value=true "${SPORE_SERVICE_TAGS_LIST[@]}"  iam, dynamodb, cloudformation create/update-stack
#   --tags "${SPORE_SERVICE_TAGS_PAIRS[@]}"                         cloudformation deploy, sam deploy
#
# plus spore_tag_role, spore_tag_table, spore_tag_log_group and
# spore_merge_bucket_tags below for
# a resource that already exists, where a create-time --tags never runs.
#
# scripts/deploy_scripts_test.go (TestServiceCreatesCarryTheServiceTags) fails a
# script that creates a role, table, log group, function, queue, bucket, API or
# stack without these.

SPORE_HOST_ROLE_KEY="spore-host:role"
SPORE_HOST_ROLE_SERVICE="service"
SPORE_HOST_COMPONENT_KEY="spore-host:component"
SPORE_HOST_DEPLOYED_BY_KEY="spore-host:deployed-by"

# The component ids. lambda-deploy-ci is the CI deploy role
# (scripts/setup-lambda-deploy-ci-role.sh), which belongs to no one Lambda.
SPORE_SERVICE_COMPONENTS="dashboard-api dashboard-websocket dashboard-websocket-processor dns-updater pipeline-orchestrator sweep-orchestrator cost-history-collector scheduler-handler rest-api spore-bot ttl-reaper lambda-deploy-ci"

case " $SPORE_SERVICE_COMPONENTS " in
  *" ${SPORE_SERVICE_COMPONENT:-} "*) ;;
  *)
    echo "ERROR: SPORE_SERVICE_COMPONENT='${SPORE_SERVICE_COMPONENT:-}' is not one of: $SPORE_SERVICE_COMPONENTS" >&2
    exit 1
    ;;
esac
case "${SPORE_SERVICE_DEPLOYED_BY:-}" in
  "" | /* | *" "* | *,* | *=*)
    echo "ERROR: SPORE_SERVICE_DEPLOYED_BY='${SPORE_SERVICE_DEPLOYED_BY:-}' must be the deploying script's repo-relative path" >&2
    exit 1
    ;;
esac

# shellcheck disable=SC2034 # read by the sourcing script
SPORE_SERVICE_TAGS_MAP="${SPORE_HOST_ROLE_KEY}=${SPORE_HOST_ROLE_SERVICE},${SPORE_HOST_COMPONENT_KEY}=${SPORE_SERVICE_COMPONENT},${SPORE_HOST_DEPLOYED_BY_KEY}=${SPORE_SERVICE_DEPLOYED_BY}"
# shellcheck disable=SC2034 # read by the sourcing script
SPORE_SERVICE_TAGS_LIST=(
  "Key=${SPORE_HOST_ROLE_KEY},Value=${SPORE_HOST_ROLE_SERVICE}"
  "Key=${SPORE_HOST_COMPONENT_KEY},Value=${SPORE_SERVICE_COMPONENT}"
  "Key=${SPORE_HOST_DEPLOYED_BY_KEY},Value=${SPORE_SERVICE_DEPLOYED_BY}"
)
# shellcheck disable=SC2034 # read by the sourcing script
SPORE_SERVICE_TAGS_PAIRS=(
  "${SPORE_HOST_ROLE_KEY}=${SPORE_HOST_ROLE_SERVICE}"
  "${SPORE_HOST_COMPONENT_KEY}=${SPORE_SERVICE_COMPONENT}"
  "${SPORE_HOST_DEPLOYED_BY_KEY}=${SPORE_SERVICE_DEPLOYED_BY}"
)

# spore_tag_role ROLE [aws options...]
# Adds the service tags to an existing IAM role. tag-role merges; it removes
# nothing.
spore_tag_role() {
  local role=$1
  shift
  aws iam tag-role --role-name "$role" --tags "${SPORE_SERVICE_TAGS_LIST[@]}" "$@"
}

# spore_tag_table TABLE REGION [aws options...]
# Adds the service tags to an existing DynamoDB table. tag-resource merges.
spore_tag_table() {
  local table=$1 region=$2
  shift 2
  local arn
  arn=$(aws dynamodb describe-table --table-name "$table" --region "$region" \
    --query Table.TableArn --output text "$@") || return 1
  aws dynamodb tag-resource --resource-arn "$arn" --tags "${SPORE_SERVICE_TAGS_LIST[@]}" \
    --region "$region" "$@"
}

# spore_tag_log_group NAME REGION ACCOUNT [aws options...]
# Adds the service tags to an existing log group. create-log-group --tags only
# applies to a group it creates, and every Lambda's group already exists after
# its first deploy, so this runs on every deploy.
spore_tag_log_group() {
  local name=$1 region=$2 account=$3
  shift 3
  aws logs tag-resource \
    --resource-arn "arn:aws:logs:${region}:${account}:log-group:${name}" \
    --tags "$SPORE_SERVICE_TAGS_MAP" \
    --region "$region" "$@"
}

# spore_merge_bucket_tags BUCKET [aws options...]
# Adds the service tags to a bucket, keeping every tag it already has.
# put-bucket-tagging REPLACES the whole tag set, so the current set is read
# first; a bucket with none answers NoSuchTagSet, and any other failure to read
# it stops here rather than writing a set that would drop the tags it could not
# see.
spore_merge_bucket_tags() {
  local bucket=$1
  shift
  local errf current merged
  errf=$(mktemp)
  if ! current=$(aws s3api get-bucket-tagging --bucket "$bucket" --query TagSet --output json "$@" 2>"$errf"); then
    if grep -q NoSuchTagSet "$errf"; then
      current='[]'
    else
      cat "$errf" >&2
      rm -f "$errf"
      echo "ERROR: could not read the tags on $bucket; not writing any" >&2
      return 1
    fi
  fi
  rm -f "$errf"
  merged=$(jq -c \
    --arg rk "$SPORE_HOST_ROLE_KEY" --arg rv "$SPORE_HOST_ROLE_SERVICE" \
    --arg ck "$SPORE_HOST_COMPONENT_KEY" --arg cv "$SPORE_SERVICE_COMPONENT" \
    --arg dk "$SPORE_HOST_DEPLOYED_BY_KEY" --arg dv "$SPORE_SERVICE_DEPLOYED_BY" \
    '[{Key: $rk, Value: $rv}, {Key: $ck, Value: $cv}, {Key: $dk, Value: $dv}] as $add
     | {TagSet: ([(. // [])[] | select(.Key as $k | [$rk, $ck, $dk] | index($k) | not)] + $add)}' \
    <<<"$current")
  aws s3api put-bucket-tagging --bucket "$bucket" --tagging "$merged" "$@"
}
