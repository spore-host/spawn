#!/bin/bash
# set -u because an unset variable here means deploying to the wrong place, and
# pipefail because a failure on the left of a pipe must not be masked.
set -euo pipefail

# Deploy the cost-history-collector Lambda (#848).
#
# Why this script exists. The function was LIVE and invoked hourly by an enabled
# EventBridge rule, last modified 2026-02-20, and the only references to it
# anywhere in the repository were its own go.mod, a .gitignore entry, a Dependabot
# entry and the changelog. There was no deploy script, no Makefile target and no
# CloudFormation or SAM template — so the source in lambda/cost-history-collector/
# could not reach the function that was running, and Dependabot had spent eight
# months bumping a module that could not ship.
#
# It is not dead code: lambda/dashboard-api/cost_history.go serves
# GET /api/cost-history from the spawn-cost-history table this fills, and that
# table holds thousands of hourly snapshots. The function works. It was simply
# unshippable.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LAMBDA_DIR="${SCRIPT_DIR}/../lambda/cost-history-collector"
FUNCTION_NAME="${SPAWN_LAMBDA_NAME:-spawn-cost-history-collector}"

# us-east-1, and deliberately NOT from AWS_REGION.
#
# The function and its hourly EventBridge rule live in us-east-1, and the
# dashboard-api that reads its output is there too. AWS_REGION is ambient — set
# for whatever you were last doing — and honouring it here is how a second
# scheduler-handler came to exist in us-west-2 while the stale one stayed in
# us-east-1 (deploy-scheduler-handler.sh records that). Override deliberately
# with SPAWN_LAMBDA_REGION, not by accident.
REGION="${SPAWN_LAMBDA_REGION:-us-east-1}"
if [ -n "${AWS_REGION:-}" ] && [ "$AWS_REGION" != "$REGION" ]; then
  echo "note: AWS_REGION=$AWS_REGION is set but ignored; deploying to $REGION" >&2
  echo "      (the hourly rule and the dashboard reader are both in $REGION;" >&2
  echo "       set SPAWN_LAMBDA_REGION to override)" >&2
fi

PROFILE="${AWS_PROFILE:-spore-host-infra}"

# The account this function belongs to, asserted rather than inferred.
#
# A profile NAME is not an account: a profile can be re-pointed, and whatever is
# in AWS_PROFILE is ambient.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"

# Matches the deployed configuration, read from the live function rather than
# guessed. x86_64 specifically — the binary must match Architectures or the
# runtime fails to start with an exec-format error.
ARCH="${SPAWN_LAMBDA_ARCH:-x86_64}"
GOARCH_FOR_ARCH="amd64"
if [ "$ARCH" = "arm64" ]; then GOARCH_FOR_ARCH="arm64"; fi
TIMEOUT="${SPAWN_LAMBDA_TIMEOUT:-300}"
MEMORY="${SPAWN_LAMBDA_MEMORY:-512}"
RUNTIME="${SPAWN_LAMBDA_RUNTIME:-provided.al2023}"
ROLE_NAME="${SPAWN_LAMBDA_ROLE:-SpawnDashboardLambdaRole}"
LOG_RETENTION_DAYS="${SPAWN_LOG_RETENTION_DAYS:-30}"

usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -r, --region REGION    AWS region (default: us-east-1)"
    echo "  -p, --profile PROFILE  AWS profile (default: spore-host-infra)"
    echo "  -f, --function NAME    Lambda function name (default: spawn-cost-history-collector)"
    echo "  -h, --help             Show this help message"
    echo ""
    echo "Environment variables:"
    echo "  AWS_PROFILE                AWS profile"
    echo "  SPAWN_LAMBDA_REGION        Override the region (AWS_REGION is ignored)"
    echo "  SPAWN_LAMBDA_ACCOUNT       Expected account id, asserted before deploying"
    echo "  SPAWN_LOG_RETENTION_DAYS   CloudWatch retention (default: 30)"
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -r|--region)   REGION="$2"; shift 2 ;;
        -p|--profile)  PROFILE="$2"; shift 2 ;;
        -f|--function) FUNCTION_NAME="$2"; shift 2 ;;
        -h|--help)     usage ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text --profile "$PROFILE")
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: profile $PROFILE is account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

echo "Deploying cost-history-collector Lambda function..."
echo "  Region:   $REGION"
echo "  Profile:  $PROFILE"
echo "  Function: $FUNCTION_NAME"
echo "  Arch:     $ARCH"
echo ""

cd "$LAMBDA_DIR"

# Build the PACKAGE (`.`), not main.go. Naming files builds only those files, so
# adding a second source file later fails outright if it is referenced — and
# would ship a binary missing it if not.
echo "🔨 Building for linux/$GOARCH_FOR_ARCH..."
GOOS=linux GOARCH="$GOARCH_FOR_ARCH" CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap .

echo "📦 Packaging..."
rm -f function.zip
zip -q function.zip bootstrap

if aws lambda get-function --function-name "$FUNCTION_NAME" \
     --region "$REGION" --profile "$PROFILE" >/dev/null 2>&1; then
    echo "♻️  Updating existing function code..."
    aws lambda update-function-code \
        --function-name "$FUNCTION_NAME" \
        --zip-file fileb://function.zip \
        --region "$REGION" \
        --profile "$PROFILE" \
        --no-cli-pager >/dev/null
    aws lambda wait function-updated \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" --profile "$PROFILE"
    echo "✅ Code updated"
else
    echo "🆕 Creating function..."
    aws lambda create-function \
        --function-name "$FUNCTION_NAME" \
        --runtime "$RUNTIME" \
        --architectures "$ARCH" \
        --role "arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}" \
        --handler bootstrap \
        --zip-file fileb://function.zip \
        --timeout "$TIMEOUT" \
        --memory-size "$MEMORY" \
        --region "$REGION" \
        --profile "$PROFILE" \
        --no-cli-pager >/dev/null
    aws lambda wait function-active \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" --profile "$PROFILE"
    echo "✅ Function created"
    echo ""
    echo "NOTE: this script does NOT create the hourly EventBridge rule."
    echo "      The live schedule is spawn-cost-history-collector-hourly, rate(1 hour)."
    echo "      A freshly created function will never be invoked until a rule targets it."
fi

# Configuration, including the RUNTIME.
#
# update-function-code does not change the runtime, and --runtime appears only in
# the create path above, which never runs for an existing function. So a script
# can declare provided.al2023 and redeploy forever while the deployed function
# stays on provided.al2 — which is exactly what happened to scheduler-handler
# (deployed 2026-01, still al2 in October, #716). Asserting it on every deploy is
# the fix, and the error is NOT swallowed: a failed configuration update must not
# be invisible, since this step now carries the runtime.
echo ""
echo "⚙️  Updating configuration (runtime, arch, timeout, memory)..."
aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --runtime "$RUNTIME" \
    --timeout "$TIMEOUT" \
    --memory-size "$MEMORY" \
    --region "$REGION" \
    --profile "$PROFILE" \
    --no-cli-pager >/dev/null
aws lambda wait function-updated \
    --function-name "$FUNCTION_NAME" \
    --region "$REGION" --profile "$PROFILE"
echo "   runtime=$RUNTIME timeout=${TIMEOUT}s memory=${MEMORY}MB"

# Stamp the version so the deployed bytes can be traced to a commit (#654/#768).
#
# `git describe` rather than a bare tag on purpose: it yields 0.127.0 on a tag,
# 0.127.0-3-gabc when ahead, and a -dirty suffix when the tree has uncommitted
# changes — so the stamp says "deployed from uncommitted code" out loud instead of
# claiming a release. This is the only thing that would have revealed that the
# running function was eight months behind its source.
SPAWN_VERSION="$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)"
aws lambda tag-resource \
    --resource "arn:aws:lambda:${REGION}:${ACCOUNT_ID}:function:${FUNCTION_NAME}" \
    --tags "spawn:managed=true,spawn:component=cost-history-collector,spawn:created-by=deploy-cost-history-collector.sh,spawn:version=$SPAWN_VERSION" \
    --region "$REGION" --profile "$PROFILE" --no-cli-pager
echo "stamped spawn:version=$SPAWN_VERSION"

# Set log retention (#653). A log group Lambda auto-creates on first invocation
# has NO retention and keeps logs forever.
# Create the group BEFORE setting its retention (#827). put-retention-policy
# creates nothing, and the group Lambda makes on first invocation has no
# retention, so a deploy that only set retention left every never-invoked
# function to start with none. Lambda writes to an existing group rather than
# making its own, so this is safe and idempotent.
aws logs create-log-group \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --region "$REGION" --profile "$PROFILE" 2>/dev/null || true  # swallow-ok: already exists; any other failure fails put-retention-policy next
# Fatal now: with the group created above, a failure here is real.
aws logs put-retention-policy \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --retention-in-days "$LOG_RETENTION_DAYS" \
  --region "$REGION" --profile "$PROFILE"
echo "log retention: ${LOG_RETENTION_DAYS}d on /aws/lambda/$FUNCTION_NAME"

rm -f bootstrap function.zip

echo ""
echo "✅ Done. Verify with:"
echo "   aws lambda get-function-configuration --function-name $FUNCTION_NAME \\"
echo "     --region $REGION --profile $PROFILE \\"
echo "     --query '{Modified:LastModified,Runtime:Runtime,Arch:Architectures}'"
