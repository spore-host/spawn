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

# Clean up
rm bootstrap dashboard-websocket-processor.zip

echo "$FUNCTION_NAME deployed successfully!"
echo ""
echo "Next steps:"
echo "1. Create event source mappings from DynamoDB Streams"
echo "2. Run: cd ../dashboard-websocket/scripts && ./setup-event-source-mappings.sh $AWS_PROFILE"
