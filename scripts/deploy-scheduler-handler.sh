#!/bin/bash
# set -u because an unset variable here means deploying to the wrong place, and
# pipefail because a failure on the left of a pipe must not be masked.
set -euo pipefail

# Script to deploy the scheduler-handler Lambda function

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LAMBDA_DIR="${SCRIPT_DIR}/../lambda/scheduler-handler"
FUNCTION_NAME="${SPAWN_LAMBDA_NAME:-scheduler-handler}"
# us-east-1, and deliberately NOT from AWS_REGION.
#
# cmd/schedule.go hardcodes us-east-1 for this function (LoadInfraAWSConfig(ctx,
# "us-east-1")), so a deployment anywhere else is unreachable by its only caller.
# AWS_REGION is ambient — set for whatever you were last doing — and honouring it
# here CREATED a second scheduler-handler in us-west-2 while the stale one stayed
# in us-east-1. Override deliberately with SPAWN_LAMBDA_REGION if you ever need
# to, not by accident.
REGION="${SPAWN_LAMBDA_REGION:-us-east-1}"
if [ -n "${AWS_REGION:-}" ] && [ "$AWS_REGION" != "$REGION" ]; then
  echo "note: AWS_REGION=$AWS_REGION is set but ignored; deploying to $REGION" >&2
  echo "      (cmd/schedule.go only looks in $REGION; set SPAWN_LAMBDA_REGION to override)" >&2
fi
PROFILE="${AWS_PROFILE:-spore-host-infra}"
# The account this function belongs to, asserted rather than inferred.
#
# A profile NAME is not an account: a profile can be re-pointed, and whatever is
# in AWS_PROFILE is ambient. The same shape of assumption about ambient config
# already created a duplicate of this function in the wrong region. Override only
# to deploy a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"

usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -r, --region REGION       AWS region (default: us-east-1)"
    echo "  -p, --profile PROFILE     AWS profile (default: spore-host-infra)"
    echo "  -f, --function NAME       Lambda function name (default: scheduler-handler)"
    echo "  -h, --help                Show this help message"
    echo ""
    echo "Environment variables:"
    echo "  AWS_REGION                AWS region"
    echo "  AWS_PROFILE               AWS profile"
    echo "  SPAWN_LAMBDA_NAME         Lambda function name"
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -r|--region)
            REGION="$2"
            shift 2
            ;;
        -p|--profile)
            PROFILE="$2"
            shift 2
            ;;
        -f|--function)
            FUNCTION_NAME="$2"
            shift 2
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo "Unknown option: $1"
            usage
            ;;
    esac
done

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text --profile "$PROFILE")
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: profile $PROFILE is account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

echo "Deploying scheduler-handler Lambda function..."
echo "  Region: $REGION"
echo "  Profile: $PROFILE"
echo "  Function: $FUNCTION_NAME"
echo ""

# Navigate to Lambda directory
cd "$LAMBDA_DIR"

# Install dependencies
echo "📦 Installing dependencies..."
go mod download

# Build Lambda function
echo "🔨 Building Lambda function..."
# CGO_ENABLED=0 so the binary is genuinely static, matching
# deploy-sweep-orchestrator.sh and deploy-custom-dns.sh. Without it, a build on a
# linux/amd64 host links against that host's glibc — which is why this function
# could sit on provided.al2 (glibc 2.26) while every other Go lambda moved to
# provided.al2023 (2.34). Static first, then the runtime bump is safe (#716).
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap .

# Create deployment package
echo "📦 Creating deployment package..."
zip -q function.zip bootstrap
rm bootstrap

# Check if function exists
if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$PROFILE" &>/dev/null; then
    echo "📤 Updating existing Lambda function..."
    aws lambda update-function-code \
        --function-name "$FUNCTION_NAME" \
        --zip-file fileb://function.zip \
        --region "$REGION" \
        --profile "$PROFILE" \
        --output table

    echo ""
    echo "⏳ Waiting for update to complete..."
    aws lambda wait function-updated \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" \
        --profile "$PROFILE"

    echo "✅ Lambda function updated successfully"
else
    echo "🆕 Creating new Lambda function..."

    ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/SpawnSchedulerHandlerExecutionRole"

    aws lambda create-function \
        --function-name "$FUNCTION_NAME" \
        --runtime provided.al2023 \
        --role "$ROLE_ARN" \
        --handler bootstrap \
        --zip-file fileb://function.zip \
        --timeout 300 \
        --memory-size 512 \
        --region "$REGION" \
        --profile "$PROFILE" \
        --description "Handles EventBridge Scheduler triggers for spawn scheduled executions" \
        --tags "Application=spawn,Component=scheduler" \
        --output table

    # Wait before the configuration update below: a freshly created function is
    # 'Pending'/'Creating' and UpdateFunctionConfiguration fails with
    # ResourceConflictException. The update path already waits; the create path
    # did not.
    aws lambda wait function-active-v2 \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" \
        --profile "$PROFILE"

    echo "✅ Lambda function created successfully"
fi

# Configuration, including the RUNTIME (#716).
#
# update-function-code does not change the runtime, and --runtime appears only in
# the create path above — which never runs for an existing function. So this
# script could declare provided.al2023 and redeploy forever while the deployed
# function stayed on provided.al2, which is exactly what happened: deployed
# 2026-01, still al2 in October. A declaration that never reaches the resource is
# the failure the runtime census catches, one level further in.
#
# The error is no longer swallowed either. This block used to end in
# `&>/dev/null || true`, so a failed configuration update was invisible — which
# is a poor property for the step that now carries the runtime.
echo ""
echo "⚙️  Updating Lambda configuration (runtime, timeout, memory)..."
# Set the environment EXPLICITLY, so the deployed configuration is reproducible
# from this script rather than inherited from compiled-in defaults (#812).
#
# The handler's defaults were wrong — it invoked `sweep-orchestrator` where the
# deployed function is `spawn-sweep-orchestrator` — and because this script set no
# environment at all, there was nothing in the deploy path to correct or even
# reveal it. Every scheduled sweep failed with ResourceNotFoundException.
#
# Named here even though the code defaults now match: a value that only exists as
# a compiled-in default cannot be read back from AWS, so skew between the repo and
# the deployed function is invisible. Stating it makes `aws lambda
# get-function-configuration` the answer rather than a guess.
#
# Note that --environment REPLACES the whole environment rather than merging.
# That is deliberate here: this script is meant to be the source of truth for
# this function's configuration, so a variable set out-of-band in the console
# SHOULD be overwritten — out-of-band config is the problem being fixed. The
# live function had no variables at all when this was written, so nothing is
# being displaced. Contrast deploy-sweep-orchestrator.sh, which once sent
# `Variables={}` and wiped a populated environment under `|| true`; the gate in
# scripts/deploy_scripts_test.go exists for that case.
#
# env-replace-ok: this script is the declarative source of truth for this
# function's configuration, and out-of-band config is the defect being fixed.
# Re-checked 2026-10-11 against the live function `scheduler-handler`: its
# environment holds exactly the two variables set below and nothing else, so the
# replacement displaces nothing.
ORCHESTRATOR_FUNCTION_NAME="${SPAWN_ORCHESTRATOR_FUNCTION_NAME:-spawn-sweep-orchestrator}"
ORCHESTRATOR_REGION="${SPAWN_ORCHESTRATOR_REGION:-us-east-1}"

# env-replace-ok: see the rationale above — this script is the declarative source
# of truth for this function's configuration, and the live function carries
# exactly the two variables set here (checked 2026-10-11).
aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --runtime provided.al2023 \
    --timeout 300 \
    --memory-size 512 \
    --environment "Variables={SPAWN_ORCHESTRATOR_FUNCTION_NAME=$ORCHESTRATOR_FUNCTION_NAME,SPAWN_ORCHESTRATOR_REGION=$ORCHESTRATOR_REGION}" \
    --region "$REGION" \
    --profile "$PROFILE" \
    --output text --query '[FunctionName,Runtime,Timeout,MemorySize]'

aws lambda wait function-updated \
    --function-name "$FUNCTION_NAME" \
    --region "$REGION" \
    --profile "$PROFILE"

# Stamp the version so an operator can read it in ONE API call (#654). Not
# CloudFormation-managed, so a tag added here is not stack drift. The create
# branch above sets Application/Component tags but no version, and the UPDATE
# path — the one that actually runs on a redeploy — set no tags at all.
SPAWN_VERSION=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)
SCHED_ARN=$(aws lambda get-function-configuration \
    --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$PROFILE" \
    --query FunctionArn --output text)
aws lambda tag-resource \
    --resource "$SCHED_ARN" \
    --tags "spawn:managed=true,spawn:component=scheduler-handler,spawn:created-by=deploy-scheduler-handler.sh,spawn:version=$SPAWN_VERSION" \
    --region "$REGION" --profile "$PROFILE" >/dev/null
echo "stamped spawn:version=$SPAWN_VERSION"

# Set log retention (#653). A log group Lambda auto-creates on first invocation
# has NO retention and keeps logs forever: a footprint audit found 10 such groups
# holding 51 MB, the only control-plane artifact measured to grow without bound.
# The three SAM-deployed functions declare a group with a retention; the
# script-deployed ones did not, which is exactly why theirs were among the ten.
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-30}"
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

echo ""
echo "✅ Deployment complete!"
echo ""
echo "Function ARN:"
aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$PROFILE" --query 'Configuration.FunctionArn' --output text
echo ""
