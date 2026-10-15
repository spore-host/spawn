#!/bin/bash
set -e

ROLE_NAME="spawnd-role"
PROFILE_NAME="spawnd-role"

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

echo "Creating IAM role and instance profile for spored agent..."

# Create trust policy for EC2
TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "ec2.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}
EOF
)

# Create role
echo "Creating IAM role: $ROLE_NAME"
aws iam create-role \
  --role-name "$ROLE_NAME" \
  --assume-role-policy-document "$TRUST_POLICY" \
  --description "IAM role for spawn EC2 instances running spored agent" \
  2>/dev/null || echo "Role already exists"

# Create inline policy for spored
POLICY_DOCUMENT=$(cat <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeTags",
        "ec2:TerminateInstances",
        "ec2:StopInstances",
        "ec2:CancelSpotInstanceRequests"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject"
      ],
      "Resource": "arn:aws:s3:::spawn-binaries-*/*/spored-*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "lambda:InvokeFunction"
      ],
      "Resource": "arn:aws:lambda:*:966362334030:function:spawn-dns-updater"
    }
  ]
}
EOF
)

echo "Attaching policy to role..."
aws iam put-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-name "SpawnInstancePolicy" \
  --policy-document "$POLICY_DOCUMENT"

# Create instance profile
echo "Creating instance profile: $PROFILE_NAME"
aws iam create-instance-profile \
  --instance-profile-name "$PROFILE_NAME" \
  2>/dev/null || echo "Instance profile already exists"

# Add role to instance profile
echo "Adding role to instance profile..."
aws iam add-role-to-instance-profile \
  --instance-profile-name "$PROFILE_NAME" \
  --role-name "$ROLE_NAME" \
  2>/dev/null || echo "Role already added to instance profile"

# Wait for IAM propagation by POLLING, not by sleeping a guess (#752).
#
# There is no `aws iam wait` for instance profiles, so the bounded poll below is
# the correct substitute. The readiness condition is not "the profile exists" —
# it is "the ROLE is attached to it", which is what EC2 needs at RunInstances and
# what the add-role call above may not have propagated yet.
echo "Waiting for the role to appear in the instance profile..."
IAM_WAITED=0
while [ "$IAM_WAITED" -lt 60 ]; do
  if aws iam get-instance-profile --instance-profile-name "$PROFILE_NAME" \
      --query "InstanceProfile.Roles[?RoleName=='$ROLE_NAME'] | length(@)" \
      --output text 2>/dev/null | grep -q '^1$'; then
    break
  fi
  sleep 2
  IAM_WAITED=$((IAM_WAITED + 2))
done
if [ "$IAM_WAITED" -ge 60 ]; then
  echo "ERROR: $ROLE_NAME did not appear in instance profile $PROFILE_NAME after ${IAM_WAITED}s." >&2
  echo "       A launch using this profile would fail; investigate before launching." >&2
  exit 1
fi
echo "Role is attached and readable after ${IAM_WAITED}s"

echo "✅ IAM role and instance profile configured successfully"
echo "Role Name: $ROLE_NAME"
echo "Instance Profile: $PROFILE_NAME"
