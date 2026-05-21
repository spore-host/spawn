#!/bin/bash
set -e

# Setup DynamoDB table for user account caching
# Usage: ./setup-user-accounts-table.sh [aws-profile]

PROFILE=${1:-default}
TABLE_NAME="spawn-user-accounts"
REGION="us-east-1"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Setting up DynamoDB Table: $TABLE_NAME"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile: $PROFILE"
echo "Region:  $REGION"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Get AWS account ID
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
echo "Account: $ACCOUNT_ID"
echo ""

# Check if table already exists
if aws dynamodb describe-table --profile "$PROFILE" --region "$REGION" --table-name "$TABLE_NAME" &>/dev/null; then
    echo -e "${YELLOW}⚠${NC}  Table already exists, skipping creation"
    TABLE_ARN=$(aws dynamodb describe-table --profile "$PROFILE" --region "$REGION" --table-name "$TABLE_NAME" --query 'Table.TableArn' --output text)
else
    # Create table
    echo "→ Creating DynamoDB table..."
    TABLE_ARN=$(aws dynamodb create-table \
        --profile "$PROFILE" \
        --region "$REGION" \
        --table-name "$TABLE_NAME" \
        --attribute-definitions \
            AttributeName=user_id,AttributeType=S \
        --key-schema \
            AttributeName=user_id,KeyType=HASH \
        --billing-mode PAY_PER_REQUEST \
        --tags \
            Key=spawn:managed,Value=true \
            Key=spawn:purpose,Value=dashboard-user-cache \
        --query 'TableDescription.TableArn' \
        --output text)

    echo -e "  ${GREEN}✓${NC} Table created: $TABLE_ARN"

    # Wait for table to be active
    echo "→ Waiting for table to be active..."
    aws dynamodb wait table-exists --profile "$PROFILE" --region "$REGION" --table-name "$TABLE_NAME"
    echo -e "  ${GREEN}✓${NC} Table is active"
fi

# Update Lambda IAM role policy to include PutItem for user account caching
echo ""
echo "→ Checking Lambda IAM policy for write permissions..."
POLICY_NAME="SpawnDashboardLambdaPolicy"
ROLE_NAME="SpawnDashboardLambdaRole"

# Create updated policy with PutItem and UpdateItem permissions
cat > /tmp/dashboard-policy-updated.json <<EOF
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
      "Sid": "WriteDynamoDBUserAccounts",
      "Effect": "Allow",
      "Action": [
        "dynamodb:PutItem",
        "dynamodb:UpdateItem"
      ],
      "Resource": "arn:aws:dynamodb:*:*:table/spawn-user-accounts"
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
    },
    {
      "Sid": "GetCallerIdentity",
      "Effect": "Allow",
      "Action": "sts:GetCallerIdentity",
      "Resource": "*"
    }
  ]
}
EOF

# Get all policy versions and delete old ones (keep only latest)
POLICY_ARN="arn:aws:iam::${ACCOUNT_ID}:policy/${POLICY_NAME}"
echo "→ Updating IAM policy..."

# Get existing policy versions and delete non-default ones to avoid hitting the 5 version limit
VERSIONS=$(aws iam list-policy-versions --profile "$PROFILE" --policy-arn "$POLICY_ARN" --query 'Versions[?!IsDefaultVersion].VersionId' --output text 2>/dev/null || echo "")
for VERSION in $VERSIONS; do
    aws iam delete-policy-version --profile "$PROFILE" --policy-arn "$POLICY_ARN" --version-id "$VERSION" 2>/dev/null || true
done

# Create new policy version and set as default
aws iam create-policy-version \
    --profile "$PROFILE" \
    --policy-arn "$POLICY_ARN" \
    --policy-document file:///tmp/dashboard-policy-updated.json \
    --set-as-default &>/dev/null || true

echo -e "  ${GREEN}✓${NC} IAM policy updated with write permissions"

# Clean up temp file
rm -f /tmp/dashboard-policy-updated.json

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ User Accounts Table Setup Complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Table ARN:"
echo "  $TABLE_ARN"
echo ""
echo "Schema:"
echo "  Primary Key: user_id (String)"
echo "  Attributes:  aws_account_id, account_base36, email, created_at, last_access"
echo "  Billing:     Pay-per-request (on-demand)"
echo ""
echo "Next Steps:"
echo "  The Lambda function will now automatically cache user account info"
echo "  on first authentication and query it on subsequent requests."
echo ""
