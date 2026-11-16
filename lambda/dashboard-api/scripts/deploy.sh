#!/bin/bash
set -e

PROFILE=${1:-spore-host-infra}
FUNCTION_NAME="spawn-dashboard-api"

# Assert the target account. Credentials are whatever happens to be ambient, so
# without this the script will happily build a parallel copy of production in a
# sandbox — or, worse, a half-copy that nothing then invokes. Added when the
# deploy-script gates were widened past scripts/ (#799) and found six scripts
# with no such check, including this one. Override only to deploy a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

# The spore-host:* service tags (spore-host/spore-host#750). Sourced before the
# cd below, because the path is relative to this script.
SPORE_SERVICE_COMPONENT=dashboard-api
SPORE_SERVICE_DEPLOYED_BY=lambda/dashboard-api/scripts/deploy.sh
# shellcheck source=scripts/lib/service-tags.sh
. "$(dirname "$0")/../../../scripts/lib/service-tags.sh"

echo "Building Lambda function..."
cd "$(dirname "$0")/.."
GOOS=linux GOARCH=amd64 go build -o bootstrap .
zip -q dashboard-api.zip bootstrap

echo "Deploying to Lambda..."
AWS_PROFILE="$PROFILE" aws lambda update-function-code \
  --function-name "$FUNCTION_NAME" \
  --zip-file fileb://dashboard-api.zip \
  --region us-east-1

echo ""
echo "✓ Deployed $FUNCTION_NAME"
echo ""
echo "Monitor logs:"
echo "  AWS_PROFILE=$PROFILE aws logs tail /aws/lambda/$FUNCTION_NAME --follow"

# Stamp spawn:version and set log retention (#799).
#
# Both on EVERY deploy, not just on create. This script tagged only on create,
# so spawn-dashboard-api carried spawn:managed and spawn:purpose but no version —
# making #768's "skew is readable in one API call" false for it, which the census
# could not report because it only globbed scripts/deploy-*.sh.
SPAWN_VERSION=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)
FUNCTION_ARN=$(aws lambda get-function-configuration --profile "$PROFILE" \
  --function-name "$FUNCTION_NAME" --region "us-east-1" \
  --query FunctionArn --output text)
aws lambda tag-resource --profile "$PROFILE" \
  --resource "$FUNCTION_ARN" \
  --tags "spawn:managed=true,spawn:component=dashboard-api,spawn:created-by=lambda/dashboard-api/scripts/deploy.sh,spawn:version=$SPAWN_VERSION,${SPORE_SERVICE_TAGS_MAP}" \
  --region "us-east-1" >/dev/null
echo "stamped spawn:version=$SPAWN_VERSION"

# A log group Lambda auto-creates on first invocation has NO retention and keeps
# logs forever (#653).
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-30}"
# Create the group BEFORE setting its retention (#827). put-retention-policy
# creates nothing, and the group Lambda makes on first invocation has no
# retention, so a deploy that only set retention left every never-invoked
# function to start with none. Lambda writes to an existing group rather than
# making its own, so this is safe and idempotent.
aws logs create-log-group \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --tags "$SPORE_SERVICE_TAGS_MAP" \
  --profile "$PROFILE" --region "us-east-1" 2>/dev/null || true  # swallow-ok: already exists; any other failure fails put-retention-policy next
# Fatal now: with the group created above, a failure here is real.
aws logs put-retention-policy --profile "$PROFILE" \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --retention-in-days "$LOG_RETENTION_DAYS" \
  --region "us-east-1"
echo "log retention: ${LOG_RETENTION_DAYS}d on /aws/lambda/$FUNCTION_NAME"
# The --tags above reach only a group this run created; an existing one is
# tagged here, on every deploy (#750).
spore_tag_log_group "/aws/lambda/$FUNCTION_NAME" "us-east-1" "$ACCOUNT_ID" --profile "$PROFILE"

# Cleanup
rm -f bootstrap dashboard-api.zip
