#!/bin/bash
set -e

# Setup API Gateway for Dashboard Lambda
# Usage: ./setup-dashboard-api-gateway.sh [aws-profile]

PROFILE=${1:-default}
FUNCTION_NAME="spawn-dashboard-api"
API_NAME="spawn-dashboard-api"
REGION="us-east-1"
DOMAIN_NAME="api.spore.host"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Setting up API Gateway for Dashboard"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile: $PROFILE"
echo "API:     $API_NAME"
echo "Domain:  $DOMAIN_NAME"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Get AWS account ID and Lambda ARN
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
LAMBDA_ARN="arn:aws:lambda:${REGION}:${ACCOUNT_ID}:function:${FUNCTION_NAME}"

echo "Account: $ACCOUNT_ID"
echo "Lambda:  $LAMBDA_ARN"
echo ""

# Check if Lambda exists
echo "→ Checking if Lambda function exists..."
aws lambda get-function --profile "$PROFILE" --region "$REGION" --function-name "$FUNCTION_NAME" &>/dev/null || {
    echo -e "${YELLOW}✗ Lambda function not found${NC}"
    echo "Run: cd .. && ./deploy.sh $PROFILE"
    exit 1
}
echo -e "  ${GREEN}✓${NC} Lambda exists"

# Create API Gateway (HTTP API - simpler and cheaper than REST API)
echo "→ Creating HTTP API Gateway..."
API_ID=$(aws apigatewayv2 create-api \
    --profile "$PROFILE" \
    --region "$REGION" \
    --name "$API_NAME" \
    --protocol-type HTTP \
    --description "Dashboard API for spawn instance viewer" \
    --cors-configuration AllowOrigins='*',AllowMethods='GET,POST,OPTIONS',AllowHeaders='Content-Type,Authorization,X-Amz-Date,X-Api-Key,X-Amz-Security-Token',AllowCredentials=false \
    --tags spawn:managed=true,spawn:purpose=dashboard-api \
    --query 'ApiId' \
    --output text 2>/dev/null || aws apigatewayv2 get-apis --profile "$PROFILE" --region "$REGION" --query "Items[?Name=='${API_NAME}'].ApiId" --output text)

echo -e "  ${GREEN}✓${NC} API created: $API_ID"

API_ENDPOINT=$(aws apigatewayv2 get-api --profile "$PROFILE" --region "$REGION" --api-id "$API_ID" --query 'ApiEndpoint' --output text)
echo -e "  ${BLUE}→${NC} Endpoint: $API_ENDPOINT"

# Create integration
echo "→ Creating Lambda integration..."
INTEGRATION_ID=$(aws apigatewayv2 create-integration \
    --profile "$PROFILE" \
    --region "$REGION" \
    --api-id "$API_ID" \
    --integration-type AWS_PROXY \
    --integration-uri "$LAMBDA_ARN" \
    --payload-format-version 2.0 \
    --query 'IntegrationId' \
    --output text 2>/dev/null || aws apigatewayv2 get-integrations --profile "$PROFILE" --region "$REGION" --api-id "$API_ID" --query 'Items[0].IntegrationId' --output text)

echo -e "  ${GREEN}✓${NC} Integration created: $INTEGRATION_ID"

# Create catch-all route
echo "→ Creating routes..."
ROUTE_ID=$(aws apigatewayv2 create-route \
    --profile "$PROFILE" \
    --region "$REGION" \
    --api-id "$API_ID" \
    --route-key '$default' \
    --target "integrations/${INTEGRATION_ID}" \
    --query 'RouteId' \
    --output text 2>/dev/null || aws apigatewayv2 get-routes --profile "$PROFILE" --region "$REGION" --api-id "$API_ID" --query "Items[?RouteKey=='\$default'].RouteId" --output text)

echo -e "  ${GREEN}✓${NC} Route created: $ROUTE_ID"

# Create stage
echo "→ Creating $default stage..."
STAGE_NAME='$default'
aws apigatewayv2 create-stage \
    --profile "$PROFILE" \
    --region "$REGION" \
    --api-id "$API_ID" \
    --stage-name "$STAGE_NAME" \
    --auto-deploy \
    --description "Default stage for dashboard API" \
    --tags spawn:managed=true &>/dev/null || true

echo -e "  ${GREEN}✓${NC} Stage deployed"

# Grant API Gateway permission to invoke Lambda
echo "→ Granting API Gateway permission to invoke Lambda..."
aws lambda add-permission \
    --profile "$PROFILE" \
    --region "$REGION" \
    --function-name "$FUNCTION_NAME" \
    --statement-id "apigateway-invoke-${API_ID}" \
    --action lambda:InvokeFunction \
    --principal apigateway.amazonaws.com \
    --source-arn "arn:aws:execute-api:${REGION}:${ACCOUNT_ID}:${API_ID}/*/*" \
    &>/dev/null || true

echo -e "  ${GREEN}✓${NC} Permission granted"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ API Gateway Setup Complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "API Details:"
echo "  API ID:   $API_ID"
echo "  Endpoint: $API_ENDPOINT"
echo ""
echo "Test the API:"
echo "  curl $API_ENDPOINT/api/user/profile"
echo ""
echo "Next Steps:"
echo "  1. Set up custom domain: ./setup-dashboard-domain.sh $PROFILE"
echo "  2. Or test with direct endpoint above"
echo ""
