#!/bin/bash
set -euo pipefail

# Setup API Gateway WebSocket API
# Usage: ./setup-websocket-api.sh <aws-profile>

if [ $# -lt 1 ]; then
    echo "Usage: $0 <aws-profile>"
    echo "Example: $0 spore-host-infra"
    exit 1
fi

AWS_PROFILE=$1
REGION="us-east-1"
API_NAME="spawn-dashboard-websocket"
LAMBDA_FUNCTION_NAME="spawn-dashboard-websocket"
STAGE_NAME="production"

echo "Setting up WebSocket API in region $REGION with profile $AWS_PROFILE"

# Check if API already exists
EXISTING_API_ID=$(aws apigatewayv2 get-apis \
    --region "$REGION" \
    --profile "$AWS_PROFILE" \
    --query "Items[?Name=='$API_NAME'].ApiId" \
    --output text 2>/dev/null || echo "")

if [ -n "$EXISTING_API_ID" ]; then
    echo "WebSocket API already exists with ID: $EXISTING_API_ID"
    API_ID=$EXISTING_API_ID
else
    # Create WebSocket API
    echo "Creating WebSocket API: $API_NAME"
    API_ID=$(aws apigatewayv2 create-api \
        --name "$API_NAME" \
        --protocol-type WEBSOCKET \
        --route-selection-expression '$request.body.action' \
        --region "$REGION" \
        --profile "$AWS_PROFILE" \
        --query 'ApiId' \
        --output text)
    echo "Created API with ID: $API_ID"
fi

# Get Lambda function ARN
LAMBDA_ARN=$(aws lambda get-function \
    --function-name "$LAMBDA_FUNCTION_NAME" \
    --region "$REGION" \
    --profile "$AWS_PROFILE" \
    --query 'Configuration.FunctionArn' \
    --output text)

echo "Lambda ARN: $LAMBDA_ARN"

# Get AWS account ID for permissions
ACCOUNT_ID=$(aws sts get-caller-identity \
    --profile "$AWS_PROFILE" \
    --query 'Account' \
    --output text)

# Create or update integration
echo "Setting up Lambda integration..."
INTEGRATION_ID=$(aws apigatewayv2 get-integrations \
    --api-id "$API_ID" \
    --region "$REGION" \
    --profile "$AWS_PROFILE" \
    --query 'Items[0].IntegrationId' \
    --output text 2>/dev/null || echo "")

if [ "$INTEGRATION_ID" = "None" ] || [ -z "$INTEGRATION_ID" ]; then
    INTEGRATION_ID=$(aws apigatewayv2 create-integration \
        --api-id "$API_ID" \
        --integration-type AWS_PROXY \
        --integration-uri "arn:aws:apigateway:$REGION:lambda:path/2015-03-31/functions/$LAMBDA_ARN/invocations" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" \
        --query 'IntegrationId' \
        --output text)
    echo "Created integration: $INTEGRATION_ID"
else
    echo "Integration already exists: $INTEGRATION_ID"
fi

# Create routes
ROUTES=('$connect' '$disconnect' '$default')

for ROUTE_KEY in "${ROUTES[@]}"; do
    echo "Setting up route: $ROUTE_KEY"

    # Check if route exists
    EXISTING_ROUTE_ID=$(aws apigatewayv2 get-routes \
        --api-id "$API_ID" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" \
        --query "Items[?RouteKey=='$ROUTE_KEY'].RouteId" \
        --output text 2>/dev/null || echo "")

    if [ -n "$EXISTING_ROUTE_ID" ] && [ "$EXISTING_ROUTE_ID" != "None" ]; then
        echo "Route $ROUTE_KEY already exists: $EXISTING_ROUTE_ID"
    else
        ROUTE_ID=$(aws apigatewayv2 create-route \
            --api-id "$API_ID" \
            --route-key "$ROUTE_KEY" \
            --target "integrations/$INTEGRATION_ID" \
            --region "$REGION" \
            --profile "$AWS_PROFILE" \
            --query 'RouteId' \
            --output text)
        echo "Created route $ROUTE_KEY: $ROUTE_ID"
    fi
done

# Grant API Gateway permission to invoke Lambda
echo "Granting API Gateway permission to invoke Lambda..."
aws lambda add-permission \
    --function-name "$LAMBDA_FUNCTION_NAME" \
    --statement-id "websocket-api-invoke-$API_ID" \
    --action lambda:InvokeFunction \
    --principal apigateway.amazonaws.com \
    --source-arn "arn:aws:execute-api:$REGION:$ACCOUNT_ID:$API_ID/*" \
    --region "$REGION" \
    --profile "$AWS_PROFILE" 2>/dev/null || echo "Permission already exists"

# Create or update stage
echo "Setting up stage: $STAGE_NAME"
EXISTING_STAGE=$(aws apigatewayv2 get-stages \
    --api-id "$API_ID" \
    --region "$REGION" \
    --profile "$AWS_PROFILE" \
    --query "Items[?StageName=='$STAGE_NAME'].StageName" \
    --output text 2>/dev/null || echo "")

if [ -n "$EXISTING_STAGE" ]; then
    echo "Stage $STAGE_NAME already exists"
else
    aws apigatewayv2 create-stage \
        --api-id "$API_ID" \
        --stage-name "$STAGE_NAME" \
        --auto-deploy \
        --region "$REGION" \
        --profile "$AWS_PROFILE"
    echo "Created stage: $STAGE_NAME"
fi

# Get WebSocket endpoint
WS_ENDPOINT="wss://$API_ID.execute-api.$REGION.amazonaws.com/$STAGE_NAME"

echo ""
echo "========================================="
echo "WebSocket API setup complete!"
echo "========================================="
echo "API ID: $API_ID"
echo "WebSocket Endpoint: $WS_ENDPOINT"
echo ""
echo "IMPORTANT: Update the following:"
echo "1. Set WEBSOCKET_ENDPOINT environment variable for dashboard-websocket-processor Lambda:"
echo "   https://$API_ID.execute-api.$REGION.amazonaws.com/$STAGE_NAME/@connections"
echo ""
echo "2. Update frontend websocket.js with WebSocket endpoint:"
echo "   const wsEndpoint = '$WS_ENDPOINT'"
echo ""
