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
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap main.go

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
aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --runtime provided.al2023 \
    --timeout 300 \
    --memory-size 512 \
    --region "$REGION" \
    --profile "$PROFILE" \
    --output text --query '[FunctionName,Runtime,Timeout,MemorySize]'

aws lambda wait function-updated \
    --function-name "$FUNCTION_NAME" \
    --region "$REGION" \
    --profile "$PROFILE"

echo ""
echo "✅ Deployment complete!"
echo ""
echo "Function ARN:"
aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$PROFILE" --query 'Configuration.FunctionArn' --output text
echo ""
