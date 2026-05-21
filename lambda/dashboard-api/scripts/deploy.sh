#!/bin/bash
set -e

PROFILE=${1:-spore-host-infra}
FUNCTION_NAME="spawn-dashboard-api"

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

# Cleanup
rm -f bootstrap dashboard-api.zip
