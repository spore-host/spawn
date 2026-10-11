#!/bin/bash
set -euo pipefail

# Deploy dashboard-websocket-processor Lambda
# Usage: ./deploy.sh <aws-profile> <websocket-api-id>

if [ $# -lt 2 ]; then
    echo "Usage: $0 <aws-profile> <websocket-api-id>"
    echo "Example: $0 spore-host-infra abc123xyz"
    echo ""
    echo "To get WebSocket API ID, run:"
    echo "aws apigatewayv2 get-apis --query \"Items[?Name=='spawn-dashboard-websocket'].ApiId\" --output text --profile <aws-profile>"
    exit 1
fi

AWS_PROFILE=$1
API_ID=$2
REGION="us-east-1"

# Assert the target account. Credentials are whatever happens to be ambient, so
# without this the script will happily build a parallel copy of production in a
# sandbox — or, worse, a half-copy that nothing then invokes. Added when the
# deploy-script gates were widened past scripts/ (#799) and found six scripts
# with no such check, including this one. Override only to deploy a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$AWS_PROFILE" --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi
FUNCTION_NAME="spawn-dashboard-websocket-processor"
ROLE_NAME="SpawnDashboardLambdaRole"
STAGE_NAME="production"

# Construct WebSocket management endpoint
WS_ENDPOINT="https://$API_ID.execute-api.$REGION.amazonaws.com/$STAGE_NAME/@connections"

echo "Building and deploying $FUNCTION_NAME to region $REGION with profile $AWS_PROFILE"
echo "WebSocket endpoint: $WS_ENDPOINT"

# Build the Lambda
echo "Building Lambda binary..."
GOOS=linux GOARCH=amd64 go build -o bootstrap .

# Create zip package
echo "Creating deployment package..."
zip dashboard-websocket-processor.zip bootstrap

# Check if function exists
if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$AWS_PROFILE" >/dev/null 2>&1; then
    echo "Updating existing function..."
    aws lambda update-function-code \
        --function-name "$FUNCTION_NAME" \
        --zip-file fileb://dashboard-websocket-processor.zip \
        --region "$REGION" \
        --profile "$AWS_PROFILE"

    echo "Waiting for function update to complete..."
    aws lambda wait function-updated \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" \
        --profile "$AWS_PROFILE"

    echo "Updating environment variables..."
    aws lambda update-function-configuration \
        --function-name "$FUNCTION_NAME" \
        --environment "Variables={WEBSOCKET_ENDPOINT=$WS_ENDPOINT}" \
        --region "$REGION" \
        --profile "$AWS_PROFILE"
else
    # Get role ARN
    ROLE_ARN=$(aws iam get-role \
        --role-name "$ROLE_NAME" \
        --profile "$AWS_PROFILE" \
        --query 'Role.Arn' \
        --output text)

    echo "Creating new function..."
    aws lambda create-function \
        --function-name "$FUNCTION_NAME" \
        --runtime provided.al2023 \
        --handler bootstrap \
        --zip-file fileb://dashboard-websocket-processor.zip \
        --role "$ROLE_ARN" \
        --timeout 60 \
        --memory-size 512 \
        --environment "Variables={WEBSOCKET_ENDPOINT=$WS_ENDPOINT}" \
        --region "$REGION" \
        --profile "$AWS_PROFILE"

    echo "Waiting for function to become active..."
    aws lambda wait function-active \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" \
        --profile "$AWS_PROFILE"
fi

# Stamp spawn:version and set log retention (#799). Both on every deploy, not
# just on create: an untagged function cannot be checked for skew, and skew is
# the normal state here because the control plane does not upgrade with the repo.
SPAWN_VERSION=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)
FUNCTION_ARN=$(aws lambda get-function-configuration \
  --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$AWS_PROFILE" \
  --query FunctionArn --output text)
aws lambda tag-resource \
  --resource "$FUNCTION_ARN" \
  --tags "spawn:managed=true,spawn:component=dashboard-websocket-processor,spawn:created-by=lambda/dashboard-websocket-processor/deploy.sh,spawn:version=$SPAWN_VERSION" \
  --region "$REGION" --profile "$AWS_PROFILE" >/dev/null
echo "stamped spawn:version=$SPAWN_VERSION"

# A log group Lambda auto-creates on first invocation has NO retention and keeps
# logs forever. put-retention-policy is idempotent and creates nothing, so a
# first deploy (group not yet created) is tolerated rather than fatal.
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-30}"
aws logs put-retention-policy \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --retention-in-days "$LOG_RETENTION_DAYS" \
  --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null \
  && echo "log retention: ${LOG_RETENTION_DAYS}d on /aws/lambda/$FUNCTION_NAME" \
  || echo "log retention: could not set it yet (group not created until first invocation)"

# Clean up
rm bootstrap dashboard-websocket-processor.zip

echo "$FUNCTION_NAME deployed successfully!"
echo ""
echo "Next steps:"
echo "1. Create event source mappings from DynamoDB Streams"
echo "2. Run: cd ../dashboard-websocket/scripts && ./setup-event-source-mappings.sh $AWS_PROFILE"
