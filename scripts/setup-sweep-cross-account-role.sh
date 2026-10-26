#!/bin/bash
set -e

ROLE_NAME="SpawnSweepCrossAccountRole"
INFRA_ACCOUNT_ID="966362334030"
CURRENT_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

# Assert the target account (#799 follow-up). This script creates a role in a
# MEMBER account — whichever account is being onboarded — so there is no correct
# default and one must not be invented. State it explicitly; the `:?` makes an
# unset value a hard failure rather than a silent deploy into whatever profile
# happened to be ambient.
EXPECTED_ACCOUNT="${SPAWN_MEMBER_ACCOUNT:?set SPAWN_MEMBER_ACCOUNT to the account ID you are onboarding}"
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
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

echo "Creating cross-account IAM role for sweep orchestration..."
echo "Current account: $CURRENT_ACCOUNT_ID"
echo "Trust: Lambda from account $INFRA_ACCOUNT_ID"

if [ "$CURRENT_ACCOUNT_ID" == "$INFRA_ACCOUNT_ID" ]; then
  echo "❌ Error: This script must be run in the target account (spore-host-dev), not the infra account"
  exit 1
fi

# Create trust policy
TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "AWS": "arn:aws:iam::${INFRA_ACCOUNT_ID}:role/SpawnSweepOrchestratorRole"
    },
    "Action": "sts:AssumeRole"
  }]
}
EOF
)

# Create role
aws iam create-role \
  --role-name "$ROLE_NAME" \
  --assume-role-policy-document "$TRUST_POLICY" \
  --description "Cross-account role for spawn sweep orchestrator to launch EC2 instances" \
  2>/dev/null || echo "Role already exists"

# Create inline policy for EC2 operations.
#
# iam:PassRole: the orchestrator launches each sweep member with an instance
# profile, because spored needs its credentials (#1323). The CLI ensures
# spored-instance-profile, whose role is spored-instance-role, and seeds it for
# every member without an iam_role of its own; spawnd-role is the profile this
# script family made before. Exactly those two roles, and only to EC2.
POLICY_DOCUMENT=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceStatus",
        "ec2:DescribeImages",
        "ec2:DescribeKeyPairs",
        "ec2:DescribeSecurityGroups",
        "ec2:DescribeSubnets",
        "ec2:DescribeVpcs",
        "ec2:CreateTags",
        "ec2:TerminateInstances"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "iam:PassRole"
      ],
      "Resource": [
        "arn:aws:iam::${CURRENT_ACCOUNT_ID}:role/spored-instance-role",
        "arn:aws:iam::${CURRENT_ACCOUNT_ID}:role/spawnd-role"
      ],
      "Condition": {
        "StringEquals": {
          "iam:PassedToService": "ec2.amazonaws.com"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": [
        "iam:CreateServiceLinkedRole"
      ],
      "Resource": "arn:aws:iam::${CURRENT_ACCOUNT_ID}:role/aws-service-role/spot.amazonaws.com/AWSServiceRoleForEC2Spot",
      "Condition": {
        "StringLike": {
          "iam:AWSServiceName": "spot.amazonaws.com"
        }
      }
    }
  ]
}
EOF
)

aws iam put-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-name "SweepCrossAccountEC2Policy" \
  --policy-document "$POLICY_DOCUMENT"

echo "✅ Cross-account IAM role $ROLE_NAME configured successfully"
echo "Role ARN: arn:aws:iam::${CURRENT_ACCOUNT_ID}:role/${ROLE_NAME}"
