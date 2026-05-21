#!/bin/bash
set -euo pipefail

# Deploy dashboard-websocket Lambda
# Usage: ./deploy.sh <aws-profile>

if [ $# -lt 1 ]; then
    echo "Usage: $0 <aws-profile>"
    echo "Example: $0 spore-host-infra"
    exit 1
fi

AWS_PROFILE=$1
REGION="us-east-1"
FUNCTION_NAME="spawn-dashboard-websocket"
ROLE_NAME="SpawnDashboardLambdaRole"

echo "Building and deploying $FUNCTION_NAME to region $REGION with profile $AWS_PROFILE"

# Build the Lambda
echo "Building Lambda binary..."
GOOS=linux GOARCH=amd64 go build -o bootstrap .

# Create zip package
echo "Creating deployment package..."
zip dashboard-websocket.zip bootstrap

# Check if function exists
if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$AWS_PROFILE" >/dev/null 2>&1; then
    echo "Updating existing function..."
    aws lambda update-function-code \
        --function-name "$FUNCTION_NAME" \
        --zip-file fileb://dashboard-websocket.zip \
        --region "$REGION" \
        --profile "$AWS_PROFILE"

    echo "Waiting for function update to complete..."
    aws lambda wait function-updated \
        --function-name "$FUNCTION_NAME" \
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
        --zip-file fileb://dashboard-websocket.zip \
        --role "$ROLE_ARN" \
        --timeout 30 \
        --memory-size 256 \
        --region "$REGION" \
        --profile "$AWS_PROFILE"

    echo "Waiting for function to become active..."
    aws lambda wait function-active \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" \
        --profile "$AWS_PROFILE"
fi

# Clean up
rm bootstrap dashboard-websocket.zip

echo "$FUNCTION_NAME deployed successfully!"
