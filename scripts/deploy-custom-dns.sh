#!/bin/bash
#
# Deploy custom DNS infrastructure for spawn (spore-host project)
#
# Usage: ./deploy-custom-dns.sh --hosted-zone-id Z123... --domain spore.example.edu
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to print colored output
info() {
    echo -e "${GREEN}✓${NC} $1"
}

warn() {
    echo -e "${YELLOW}⚠${NC} $1"
}

error() {
    echo -e "${RED}✗${NC} $1"
}

# Parse command line arguments
HOSTED_ZONE_ID=""
DOMAIN=""
REGION=$(aws configure get region || echo "us-east-1")

while [[ $# -gt 0 ]]; do
  case $1 in
    --hosted-zone-id)
      HOSTED_ZONE_ID="$2"
      shift 2
      ;;
    --domain)
      DOMAIN="$2"
      shift 2
      ;;
    --region)
      REGION="$2"
      shift 2
      ;;
    --help)
      echo "Usage: $0 --hosted-zone-id Z123... --domain spore.example.edu [--region us-east-1]"
      echo ""
      echo "Options:"
      echo "  --hosted-zone-id    Route53 hosted zone ID (required)"
      echo "  --domain            DNS domain (required)"
      echo "  --region            AWS region (default: from AWS CLI config)"
      echo "  --help              Show this help message"
      exit 0
      ;;
    *)
      error "Unknown option: $1"
      echo "Use --help for usage information"
      exit 1
      ;;
  esac
done

# Validate required parameters
if [ -z "$HOSTED_ZONE_ID" ] || [ -z "$DOMAIN" ]; then
  error "Missing required parameters"
  echo ""
  echo "Usage: $0 --hosted-zone-id Z123... --domain spore.example.edu"
  echo "Use --help for more information"
  exit 1
fi

echo "========================================="
echo "  Spawn Custom DNS Deployment"
echo "========================================="
echo ""
echo "Configuration:"
echo "  Hosted Zone: $HOSTED_ZONE_ID"
echo "  Domain:      $DOMAIN"
echo "  Region:      $REGION"
echo ""

# Get AWS account ID
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
info "AWS Account: $ACCOUNT_ID"
echo ""

# Step 1: Build Lambda function
echo "Step 1: Building Lambda function..."
cd lambda/dns-updater

if [ ! -f "main.go" ]; then
  error "Lambda source not found: lambda/dns-updater/main.go"
  exit 1
fi

info "Building for Linux/AMD64..."
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go

info "Creating deployment package..."
zip -q function.zip bootstrap
cd ../..

info "Lambda function built"
echo ""

# Step 2: Create IAM role
echo "Step 2: Creating IAM role..."

ROLE_NAME="SpawnDNSLambdaRole"

# Check if role exists
if aws iam get-role --role-name "$ROLE_NAME" &>/dev/null; then
  warn "IAM role $ROLE_NAME already exists"
else
  info "Creating IAM role: $ROLE_NAME"
  aws iam create-role \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document file://deployment/trust-policy.json \
    --description "Execution role for spawn DNS Lambda function" \
    > /dev/null

  # Wait for role to be available
  sleep 3
  info "IAM role created"
fi

# Get role ARN
ROLE_ARN=$(aws iam get-role --role-name "$ROLE_NAME" --query 'Role.Arn' --output text)
info "Role ARN: $ROLE_ARN"
echo ""

# Step 3: Create/update IAM policy
echo "Step 3: Creating IAM policy..."

# Create policy document
cat > /tmp/spawn-lambda-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Route53Permissions",
      "Effect": "Allow",
      "Action": [
        "route53:ChangeResourceRecordSets",
        "route53:ListResourceRecordSets"
      ],
      "Resource": "arn:aws:route53:::hostedzone/$HOSTED_ZONE_ID"
    },
    {
      "Sid": "EC2Permissions",
      "Effect": "Allow",
      "Action": "ec2:DescribeInstances",
      "Resource": "*"
    },
    {
      "Sid": "CloudWatchLogsPermissions",
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogGroup",
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "arn:aws:logs:*:*:*"
    }
  ]
}
EOF

info "Attaching inline policy to role..."
aws iam put-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-name "SpawnDNSPermissions" \
  --policy-document file:///tmp/spawn-lambda-policy.json

rm /tmp/spawn-lambda-policy.json
info "IAM policy attached"
echo ""

# Step 4: Create or update Lambda function
echo "Step 4: Deploying Lambda function..."

FUNCTION_NAME="spawn-dns-updater"

# Check if function exists
if aws lambda get-function --function-name "$FUNCTION_NAME" &>/dev/null; then
  warn "Lambda function $FUNCTION_NAME already exists, updating code..."

  aws lambda update-function-code \
    --function-name "$FUNCTION_NAME" \
    --zip-file fileb://lambda/dns-updater/function.zip \
    > /dev/null

  info "Updating configuration..."
  aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --environment Variables="{HOSTED_ZONE_ID=$HOSTED_ZONE_ID,DOMAIN=$DOMAIN}" \
    > /dev/null

  info "Lambda function updated"
else
  info "Creating Lambda function: $FUNCTION_NAME"

  aws lambda create-function \
    --function-name "$FUNCTION_NAME" \
    --runtime provided.al2023 \
    --role "$ROLE_ARN" \
    --handler bootstrap \
    --zip-file fileb://lambda/dns-updater/function.zip \
    --timeout 30 \
    --memory-size 256 \
    --environment Variables="{HOSTED_ZONE_ID=$HOSTED_ZONE_ID,DOMAIN=$DOMAIN}" \
    --description "DNS updater for spawn managed instances" \
    > /dev/null

  info "Lambda function created"
fi

# Get Lambda ARN
LAMBDA_ARN=$(aws lambda get-function \
  --function-name "$FUNCTION_NAME" \
  --query 'Configuration.FunctionArn' \
  --output text)

info "Lambda ARN: $LAMBDA_ARN"
echo ""

# Step 5: Create API Gateway
echo "Step 5: Creating API Gateway..."

API_NAME="spawn-dns-api"

# Check if API already exists
API_ID=$(aws apigateway get-rest-apis \
  --query "items[?name=='$API_NAME'].id" \
  --output text)

if [ -z "$API_ID" ]; then
  info "Creating REST API: $API_NAME"

  API_ID=$(aws apigateway create-rest-api \
    --name "$API_NAME" \
    --description "DNS update API for spawn instances" \
    --query 'id' \
    --output text)

  info "API created: $API_ID"
else
  warn "API Gateway $API_NAME already exists: $API_ID"
fi

# Get root resource ID
ROOT_ID=$(aws apigateway get-resources \
  --rest-api-id "$API_ID" \
  --query 'items[?path==`/`].id' \
  --output text)

info "Root resource: $ROOT_ID"

# Check if /update-dns resource exists
RESOURCE_ID=$(aws apigateway get-resources \
  --rest-api-id "$API_ID" \
  --query "items[?pathPart=='update-dns'].id" \
  --output text)

if [ -z "$RESOURCE_ID" ]; then
  info "Creating /update-dns resource..."

  RESOURCE_ID=$(aws apigateway create-resource \
    --rest-api-id "$API_ID" \
    --parent-id "$ROOT_ID" \
    --path-part "update-dns" \
    --query 'id' \
    --output text)

  info "Resource created: $RESOURCE_ID"
else
  warn "Resource /update-dns already exists: $RESOURCE_ID"
fi

# Create POST method
if ! aws apigateway get-method \
  --rest-api-id "$API_ID" \
  --resource-id "$RESOURCE_ID" \
  --http-method POST &>/dev/null; then

  info "Creating POST method..."

  aws apigateway put-method \
    --rest-api-id "$API_ID" \
    --resource-id "$RESOURCE_ID" \
    --http-method POST \
    --authorization-type NONE \
    --no-api-key-required \
    > /dev/null

  info "POST method created"
else
  warn "POST method already exists"
fi

# Create Lambda integration
info "Creating Lambda integration..."

aws apigateway put-integration \
  --rest-api-id "$API_ID" \
  --resource-id "$RESOURCE_ID" \
  --http-method POST \
  --type AWS_PROXY \
  --integration-http-method POST \
  --uri "arn:aws:apigateway:$REGION:lambda:path/2015-03-31/functions/$LAMBDA_ARN/invocations" \
  > /dev/null

info "Lambda integration created"

# Grant API Gateway permission to invoke Lambda
info "Granting API Gateway permission to invoke Lambda..."

# Remove old permission if exists
aws lambda remove-permission \
  --function-name "$FUNCTION_NAME" \
  --statement-id apigateway-invoke &>/dev/null || true

# Add new permission
aws lambda add-permission \
  --function-name "$FUNCTION_NAME" \
  --statement-id apigateway-invoke \
  --action lambda:InvokeFunction \
  --principal apigateway.amazonaws.com \
  --source-arn "arn:aws:execute-api:$REGION:$ACCOUNT_ID:$API_ID/*/*" \
  > /dev/null

info "Permission granted"
echo ""

# Step 6: Deploy API
echo "Step 6: Deploying API to production..."

aws apigateway create-deployment \
  --rest-api-id "$API_ID" \
  --stage-name prod \
  --description "Deployment $(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  > /dev/null

API_ENDPOINT="https://$API_ID.execute-api.$REGION.amazonaws.com/prod/update-dns"

info "API deployed to: $API_ENDPOINT"
echo ""

# Summary
echo "========================================="
echo "  Deployment Complete!"
echo "========================================="
echo ""
echo "Configuration:"
echo "  Domain:       $DOMAIN"
echo "  Hosted Zone:  $HOSTED_ZONE_ID"
echo "  API Endpoint: $API_ENDPOINT"
echo ""
echo "Next Steps:"
echo ""
echo "1. Verify DNS delegation (if not already done):"
echo "   dig NS $DOMAIN"
echo ""
echo "2. Configure SSM parameters for auto-discovery:"
echo "   aws ssm put-parameter \\"
echo "     --name \"/spawn/dns/domain\" \\"
echo "     --value \"$DOMAIN\" \\"
echo "     --type String"
echo ""
echo "   aws ssm put-parameter \\"
echo "     --name \"/spawn/dns/api_endpoint\" \\"
echo "     --value \"$API_ENDPOINT\" \\"
echo "     --type String"
echo ""
echo "3. Test the deployment:"
echo "   spawn launch --dns test --ttl 1h"
echo ""
echo "4. (Optional) Enable DNSSEC:"
echo "   ./scripts/enable-dnssec.sh --hosted-zone-id $HOSTED_ZONE_ID --domain $DOMAIN"
echo ""
echo "For more information, see CUSTOM_DNS.md"
echo ""
