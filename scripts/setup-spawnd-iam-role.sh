#!/bin/bash
set -e

ROLE_NAME="spawnd-role"
PROFILE_NAME="spawnd-role"

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
        "ec2:StopInstances"
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

# Wait for IAM propagation
echo "Waiting for IAM propagation (10 seconds)..."
sleep 10

echo "✅ IAM role and instance profile configured successfully"
echo "Role Name: $ROLE_NAME"
echo "Instance Profile: $PROFILE_NAME"
