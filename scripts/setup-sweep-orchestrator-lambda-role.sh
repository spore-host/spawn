#!/bin/bash
set -e

ROLE_NAME="SpawnSweepOrchestratorRole"
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGION="us-east-1"

echo "Creating IAM role for Lambda sweep orchestrator..."

# Create trust policy
TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "lambda.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}
EOF
)

# Create role
aws iam create-role \
  --role-name "$ROLE_NAME" \
  --assume-role-policy-document "$TRUST_POLICY" \
  --description "Role for spawn sweep orchestrator Lambda function" \
  2>/dev/null || echo "Role already exists"

# Attach basic Lambda execution policy
aws iam attach-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-arn "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"

# Create inline policy for sweep orchestration
POLICY_DOCUMENT=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:Query"
      ],
      "Resource": [
        "arn:aws:dynamodb:${REGION}:${ACCOUNT_ID}:table/spawn-sweep-orchestration",
        "arn:aws:dynamodb:${REGION}:${ACCOUNT_ID}:table/spawn-sweep-orchestration/index/*"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject"
      ],
      "Resource": "arn:aws:s3:::spawn-sweeps-${REGION}/sweeps/*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "lambda:InvokeFunction"
      ],
      "Resource": "arn:aws:lambda:${REGION}:${ACCOUNT_ID}:function:spawn-sweep-orchestrator"
    },
    {
      "Effect": "Allow",
      "Action": [
        "sts:AssumeRole"
      ],
      "Resource": "arn:aws:iam::*:role/SpawnSweepCrossAccountRole"
    }
  ]
}
EOF
)

aws iam put-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-name "SweepOrchestratorPolicy" \
  --policy-document "$POLICY_DOCUMENT"

echo "✅ IAM role $ROLE_NAME configured successfully"
echo "Role ARN: arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"
