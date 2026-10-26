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

# The trust names the dashboard's Cognito authenticated role, the identity the
# browser (spore-host web/js/main.js) and dashboard-api (#815) assume this role
# with. It used to name the whole infra account's root, which let any principal
# there that was granted sts:AssumeRole read this account; the role production
# runs with names Cognito_SpawnDashboard_Auth_Role (lambda/dashboard-api/cross_account.go).
TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::${INFRA_ACCOUNT_ID}:role/Cognito_SpawnDashboard_Auth_Role"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF
)

if aws iam get-role --profile "$PROFILE" --role-name "$ROLE_NAME" >/dev/null 2>&1; then
  aws iam update-assume-role-policy \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --policy-document "$TRUST_POLICY"
else
  aws iam create-role \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document "$TRUST_POLICY" \
    --description "Cross-account read access for spawn dashboard API" \
    --tags Key=spawn:managed,Value=true Key=spawn:purpose,Value=dashboard-read
fi

# What the dashboard reads in this account, and only that (#1344): every EC2
# call made on this role, by dashboard-api and by the browser, is
# DescribeInstances. Every signed-in dashboard user can assume this role
# directly, so it must stay read-only and narrow. It used to carry the
# AWS-managed AmazonEC2ReadOnlyAccess and AmazonDynamoDBReadOnlyAccess, which
# let any of them read every EC2 resource and every DynamoDB table here. Same
# inline policy name as spore-host's scripts/setup-dashboard-cross-account-role.sh,
# so both converge on one document. Terminate is deliberately NOT granted:
# see spawn#1349.
READ_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DescribeInstances",
      "Effect": "Allow",
      "Action": ["ec2:DescribeInstances"],
      "Resource": "*"
    }
  ]
}
EOF
)
aws iam put-role-policy \
  --profile "$PROFILE" \
  --role-name "$ROLE_NAME" \
  --policy-name "SpawnDashboardEC2ReadAccess" \
  --policy-document "$READ_POLICY"

# Detach the AWS-managed policies earlier versions attached.
for MANAGED in AmazonEC2ReadOnlyAccess AmazonDynamoDBReadOnlyAccess; do
  ARN="arn:aws:iam::aws:policy/$MANAGED"
  if aws iam list-attached-role-policies --profile "$PROFILE" --role-name "$ROLE_NAME" \
      --query "AttachedPolicies[?PolicyArn=='$ARN'].PolicyArn" --output text | grep -q .; then
    aws iam detach-role-policy --profile "$PROFILE" --role-name "$ROLE_NAME" --policy-arn "$ARN"
    echo "✓ Detached $MANAGED"
  fi
done

echo "✓ Cross-account role ready: arn:aws:iam::${ACCOUNT_ID}:role/$ROLE_NAME"
echo ""
echo "Verify with:"
echo "  aws iam get-role --profile $PROFILE --role-name $ROLE_NAME"
