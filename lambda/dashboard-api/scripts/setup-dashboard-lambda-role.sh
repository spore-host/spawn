#!/bin/bash
set -e

# Setup IAM Role for Dashboard Lambda
# Usage: ./setup-dashboard-lambda-role.sh [aws-profile]

PROFILE=${1:-default}
ROLE_NAME="SpawnDashboardLambdaRole"
POLICY_NAME="SpawnDashboardLambdaPolicy"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Setting up IAM Role for Dashboard Lambda"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile: $PROFILE"
echo "Role:    $ROLE_NAME"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Get AWS account ID
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
echo "Account: $ACCOUNT_ID"
echo ""

# Create trust policy
echo "→ Creating trust policy..."
cat > /tmp/trust-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "lambda.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF

# Create IAM role
echo "→ Creating IAM role..."
ROLE_ARN=$(aws iam create-role \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document file:///tmp/trust-policy.json \
    --description "Dashboard API Lambda execution role - read-only EC2/DynamoDB access" \
    --tags Key=spawn:managed,Value=true Key=spawn:purpose,Value=dashboard-api \
    --query 'Role.Arn' \
    --output text 2>/dev/null || aws iam get-role --profile "$PROFILE" --role-name "$ROLE_NAME" --query 'Role.Arn' --output text)

echo -e "  ${GREEN}✓${NC} Role created: $ROLE_ARN"

# Attach basic Lambda execution policy
echo "→ Attaching Lambda basic execution policy..."
aws iam attach-role-policy \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --policy-arn "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole" 2>/dev/null || true

echo -e "  ${GREEN}✓${NC} Basic execution policy attached"

# Create custom policy for dashboard access
echo "→ Creating custom policy..."
cat > /tmp/dashboard-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadEC2Instances",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeRegions",
        "ec2:DescribeTags"
      ],
      "Resource": "*"
    },
    {
      "Sid": "ReadDynamoDB",
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:BatchGetItem",
        "dynamodb:Query",
        "dynamodb:Scan"
      ],
      "Resource": [
        "arn:aws:dynamodb:*:*:table/spawn-user-accounts",
        "arn:aws:dynamodb:*:*:table/spawn-sweep-orchestration",
        "arn:aws:dynamodb:*:*:table/spawn-autoscale-groups-production",
        "arn:aws:dynamodb:*:*:table/spawn-registry-production"
      ]
    },
    {
      "Sid": "AssumeRoleForCrossAccount",
      "Effect": "Allow",
      "Action": "sts:AssumeRole",
      "Resource": "arn:aws:iam::942542972736:role/SpawnDashboardCrossAccountRole"
    },
    {
      "Sid": "CancelSweeps",
      "Effect": "Allow",
      "Action": [
        "dynamodb:UpdateItem"
      ],
      "Resource": "arn:aws:dynamodb:*:*:table/spawn-sweep-orchestration"
    }
  ]
}
EOF

POLICY_ARN=$(aws iam create-policy \
    --profile "$PROFILE" \
    --policy-name "$POLICY_NAME" \
    --policy-document file:///tmp/dashboard-policy.json \
    --description "Dashboard API Lambda permissions for read-only access" \
    --tags Key=spawn:managed,Value=true Key=spawn:purpose,Value=dashboard-api \
    --query 'Policy.Arn' \
    --output text 2>/dev/null || echo "arn:aws:iam::${ACCOUNT_ID}:policy/${POLICY_NAME}")

echo -e "  ${GREEN}✓${NC} Policy created: $POLICY_ARN"

# Attach custom policy
echo "→ Attaching custom policy..."
aws iam attach-role-policy \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --policy-arn "$POLICY_ARN" 2>/dev/null || true

echo -e "  ${GREEN}✓${NC} Custom policy attached"

# Clean up temp files
rm -f /tmp/trust-policy.json /tmp/dashboard-policy.json

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ IAM Role Setup Complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Role ARN:"
echo "  $ROLE_ARN"
echo ""
echo "Next Steps:"
echo "  1. Deploy Lambda function: cd .. && ./deploy.sh $PROFILE"
echo "  2. Set up API Gateway: ./scripts/setup-dashboard-api-gateway.sh $PROFILE"
echo ""
