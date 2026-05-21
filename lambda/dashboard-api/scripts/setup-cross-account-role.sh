#!/bin/bash
set -e

PROFILE=${1:-spore-host-dev}
ROLE_NAME="SpawnDashboardCrossAccountReadRole"
INFRA_ACCOUNT_ID="966362334030"

echo "Setting up cross-account role in dev account..."

# Create trust policy document
TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::${INFRA_ACCOUNT_ID}:root"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF
)

# Create role
aws iam create-role \
  --profile "$PROFILE" \
  --role-name "$ROLE_NAME" \
  --assume-role-policy-document "$TRUST_POLICY" \
  --description "Cross-account read access for spawn dashboard API" \
  2>/dev/null || echo "Role already exists"

# Attach read-only EC2 policy
aws iam attach-role-policy \
  --profile "$PROFILE" \
  --role-name "$ROLE_NAME" \
  --policy-arn "arn:aws:iam::aws:policy/AmazonEC2ReadOnlyAccess" \
  2>/dev/null || true

# Attach read-only DynamoDB policy
aws iam attach-role-policy \
  --profile "$PROFILE" \
  --role-name "$ROLE_NAME" \
  --policy-arn "arn:aws:iam::aws:policy/AmazonDynamoDBReadOnlyAccess" \
  2>/dev/null || true

echo "✓ Cross-account role created: arn:aws:iam::435415984226:role/$ROLE_NAME"
echo ""
echo "Verify with:"
echo "  aws iam get-role --profile $PROFILE --role-name $ROLE_NAME"
