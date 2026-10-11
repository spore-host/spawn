#!/bin/bash
set -e

PROFILE=${1:-spore-host-dev}
ROLE_NAME="SpawnDashboardCrossAccountReadRole"
INFRA_ACCOUNT_ID="966362334030"

# Assert the target account (#799 follow-up). This script creates a role in a
# MEMBER account — whichever account is being onboarded — so there is no correct
# default and one must not be invented. State it explicitly; the `:?` makes an
# unset value a hard failure rather than a silent deploy into whatever profile
# happened to be ambient.
EXPECTED_ACCOUNT="${SPAWN_MEMBER_ACCOUNT:?set SPAWN_MEMBER_ACCOUNT to the account ID you are onboarding}"
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Point AWS_PROFILE at the member account, or correct SPAWN_MEMBER_ACCOUNT." >&2
  exit 1
fi
if [ "$ACCOUNT_ID" = "966362334030" ]; then
  echo "ERROR: refusing to create a cross-account role inside the infra account." >&2
  echo "       It would trust itself, which means the profile is aimed at the wrong account." >&2
  exit 1
fi

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
 

# Attach read-only DynamoDB policy
aws iam attach-role-policy \
  --profile "$PROFILE" \
  --role-name "$ROLE_NAME" \
  --policy-arn "arn:aws:iam::aws:policy/AmazonDynamoDBReadOnlyAccess" \
 

echo "✓ Cross-account role created: arn:aws:iam::435415984226:role/$ROLE_NAME"
echo ""
echo "Verify with:"
echo "  aws iam get-role --profile $PROFILE --role-name $ROLE_NAME"
