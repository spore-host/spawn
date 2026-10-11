#!/bin/bash
set -e

# Deploy Dashboard API Lambda function
# Usage: ./deploy.sh [aws-profile]

PROFILE=${1:-default}
FUNCTION_NAME="spawn-dashboard-api"
ROLE_NAME="SpawnDashboardLambdaRole"
REGION="us-east-1"

# Assert the target account. Credentials are whatever happens to be ambient, so
# without this the script will happily build a parallel copy of production in a
# sandbox — or, worse, a half-copy that nothing then invokes. Added when the
# deploy-script gates were widened past scripts/ (#799) and found six scripts
# with no such check, including this one. Override only to deploy a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

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

# Remove build artifacts however this script exits, including on the error paths
# below. Registered before the first thing that can fail.
cleanup() { rm -f bootstrap function.zip; }
trap cleanup EXIT

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

    # Lambda rejects a second mutation while the first is still in flight. This
    # script went straight from update-function-code to
    # update-function-configuration and reliably hit
    # ResourceConflictException — exit 254 with the CODE updated and the
    # configuration not, which looks like total failure and is not (#799).
    echo "→ Waiting for the code update to settle..."
    aws lambda wait function-updated \
        --profile "$PROFILE" \
        --region "$REGION" \
        --function-name "$FUNCTION_NAME"

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

# Stamp spawn:version and set log retention (#799).
#
# Both on EVERY deploy, not just on create. This script tagged only on create,
# so spawn-dashboard-api carried spawn:managed and spawn:purpose but no version —
# making #768's "skew is readable in one API call" false for it, which the census
# could not report because it only globbed scripts/deploy-*.sh.
SPAWN_VERSION=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)
FUNCTION_ARN=$(aws lambda get-function-configuration --profile "$PROFILE" \
  --function-name "$FUNCTION_NAME" --region "$REGION" \
  --query FunctionArn --output text)
aws lambda tag-resource --profile "$PROFILE" \
  --resource "$FUNCTION_ARN" \
  --tags "spawn:managed=true,spawn:component=dashboard-api,spawn:created-by=lambda/dashboard-api/deploy.sh,spawn:version=$SPAWN_VERSION" \
  --region "$REGION" >/dev/null
echo "stamped spawn:version=$SPAWN_VERSION"

# A log group Lambda auto-creates on first invocation has NO retention and keeps
# logs forever. put-retention-policy is idempotent and creates nothing, so a
# first deploy (group not yet created) is tolerated rather than fatal.
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-30}"
aws logs put-retention-policy --profile "$PROFILE" \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --retention-in-days "$LOG_RETENTION_DAYS" \
  --region "$REGION" 2>/dev/null \
  && echo "log retention: ${LOG_RETENTION_DAYS}d on /aws/lambda/$FUNCTION_NAME" \
  || echo "log retention: could not set it yet (group not created until first invocation)"

# Clean up. Via trap because `set -e` skipped this line on the failure mode above
# and left a 20 MB binary and a 6.6 MB zip behind — a script cannot be trusted to
# tidy up only when it succeeds (#799).
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
