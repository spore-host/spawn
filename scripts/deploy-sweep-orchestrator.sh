#!/bin/bash
set -e

FUNCTION_NAME="spawn-sweep-orchestrator"
ROLE_NAME="SpawnSweepOrchestratorRole"
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGION="us-east-1"
LAMBDA_DIR="../lambda/sweep-orchestrator"

echo "Building and deploying Lambda function: $FUNCTION_NAME"

# Navigate to Lambda directory
cd "$(dirname "$0")/$LAMBDA_DIR"

# Build Go binary for Lambda
echo "Building Go binary..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap main.go

# Create deployment package
echo "Creating deployment package..."
zip -q function.zip bootstrap
rm bootstrap

# Check if function exists
FUNCTION_EXISTS=$(aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" 2>/dev/null || echo "")

if [ -z "$FUNCTION_EXISTS" ]; then
  echo "Creating new Lambda function..."
  aws lambda create-function \
    --function-name "$FUNCTION_NAME" \
    --runtime provided.al2023 \
    --role "arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}" \
    --handler bootstrap \
    --zip-file fileb://function.zip \
    --timeout 900 \
    --memory-size 512 \
    --region "$REGION" \
    --environment "Variables={}"
else
  echo "Updating existing Lambda function..."
  aws lambda update-function-code \
    --function-name "$FUNCTION_NAME" \
    --zip-file fileb://function.zip \
    --region "$REGION"

  # Update function configuration
  aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --timeout 900 \
    --memory-size 512 \
    --region "$REGION" \
    --environment "Variables={}" \
    || true
fi

# Clean up
rm function.zip

echo "✅ Lambda function $FUNCTION_NAME deployed successfully"
echo "Function ARN: arn:aws:lambda:${REGION}:${ACCOUNT_ID}:function:${FUNCTION_NAME}"
