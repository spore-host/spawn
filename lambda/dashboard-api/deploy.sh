#!/bin/bash
set -e

# Deploy Dashboard API Lambda function
# Usage: ./deploy.sh [aws-profile]

PROFILE=${1:-default}
FUNCTION_NAME="spawn-dashboard-api"
ROLE_NAME="SpawnDashboardLambdaRole"
REGION="us-east-1"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Deploying Dashboard API Lambda Function"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile:  $PROFILE"
echo "Function: $FUNCTION_NAME"
echo "Region:   $REGION"
echo ""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Check if role exists
echo "→ Checking IAM role..."
ROLE_ARN=$(aws iam get-role --profile "$PROFILE" --role-name "$ROLE_NAME" --query 'Role.Arn' --output text 2>/dev/null || echo "")
if [ -z "$ROLE_ARN" ]; then
    echo -e "${RED}✗ IAM role not found: $ROLE_NAME${NC}"
    echo "Run: ./scripts/setup-dashboard-lambda-role.sh $PROFILE"
    exit 1
fi
echo -e "  ${GREEN}✓${NC} Role exists: $ROLE_ARN"

# Build Lambda binary
echo "→ Building Lambda binary for Linux..."
GOOS=linux GOARCH=amd64 go build -o bootstrap -ldflags="-s -w" .
if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Build failed${NC}"
    exit 1
fi
echo -e "  ${GREEN}✓${NC} Binary built"

# Create deployment package
echo "→ Creating deployment package..."
zip -q function.zip bootstrap
echo -e "  ${GREEN}✓${NC} Package created: function.zip"

# Check if function exists
echo "→ Checking if Lambda function exists..."
FUNCTION_EXISTS=$(aws lambda get-function --profile "$PROFILE" --region "$REGION" --function-name "$FUNCTION_NAME" 2>/dev/null || echo "")

if [ -z "$FUNCTION_EXISTS" ]; then
    # Create new function
    echo "→ Creating new Lambda function..."
    aws lambda create-function \
        --profile "$PROFILE" \
        --region "$REGION" \
        --function-name "$FUNCTION_NAME" \
        --runtime provided.al2023 \
        --role "$ROLE_ARN" \
        --handler bootstrap \
        --zip-file fileb://function.zip \
        --timeout 30 \
        --memory-size 512 \
        --description "Spawn Dashboard API - read-only instance viewer" \
        --tags spawn:managed=true,spawn:purpose=dashboard-api \
        --output json > /dev/null

    echo -e "  ${GREEN}✓${NC} Function created"
else
    # Update existing function
    echo "→ Updating existing Lambda function..."
    aws lambda update-function-code \
        --profile "$PROFILE" \
        --region "$REGION" \
        --function-name "$FUNCTION_NAME" \
        --zip-file fileb://function.zip \
        --output json > /dev/null

    echo -e "  ${GREEN}✓${NC} Function code updated"

    # Update configuration
    echo "→ Updating function configuration..."
    aws lambda update-function-configuration \
        --profile "$PROFILE" \
        --region "$REGION" \
        --function-name "$FUNCTION_NAME" \
        --timeout 30 \
        --memory-size 512 \
        --output json > /dev/null

    echo -e "  ${GREEN}✓${NC} Configuration updated"
fi

# Clean up
rm -f bootstrap function.zip
echo "→ Cleaned up build artifacts"

# Get function ARN
FUNCTION_ARN=$(aws lambda get-function --profile "$PROFILE" --region "$REGION" --function-name "$FUNCTION_NAME" --query 'Configuration.FunctionArn' --output text)

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ Deployment Complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Function Details:"
echo "  Name:    $FUNCTION_NAME"
echo "  Runtime: provided.al2023 (Go)"
echo "  Timeout: 30 seconds"
echo "  Memory:  512 MB"
echo ""
echo "Function ARN:"
echo "  $FUNCTION_ARN"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Next Steps:"
echo "  1. Create API Gateway: ./setup-dashboard-api-gateway.sh"
echo "  2. Test the Lambda directly:"
echo "     aws lambda invoke --function-name $FUNCTION_NAME \\"
echo "       --payload '{\"path\":\"/api/instances\",\"httpMethod\":\"GET\"}' \\"
echo "       response.json"
echo ""
