#!/bin/bash
set -e

EMAIL=${1:-scttfrdmn@gmail.com}
PROFILE=${2:-spore-host-infra}
TABLE_NAME="spawn-user-accounts"

# scott-admin constants
CLI_IAM_ARN="arn:aws:iam::435415984226:user/scott-admin"
AWS_ACCOUNT_ID="435415984226"
ACCOUNT_BASE36="c8s8u"

TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)

echo "Creating user mapping for $EMAIL..."

aws dynamodb put-item \
  --profile "$PROFILE" \
  --region us-east-1 \
  --table-name "$TABLE_NAME" \
  --item "{
    \"user_id\": {\"S\": \"$CLI_IAM_ARN\"},
    \"cli_iam_arn\": {\"S\": \"$CLI_IAM_ARN\"},
    \"email\": {\"S\": \"$EMAIL\"},
    \"identity_type\": {\"S\": \"cli\"},
    \"aws_account_id\": {\"S\": \"$AWS_ACCOUNT_ID\"},
    \"account_base36\": {\"S\": \"$ACCOUNT_BASE36\"},
    \"linked_at\": {\"S\": \"$TIMESTAMP\"},
    \"created_at\": {\"S\": \"$TIMESTAMP\"},
    \"last_access\": {\"S\": \"$TIMESTAMP\"}
  }"

echo "✓ User mapping created for $EMAIL → $CLI_IAM_ARN"
echo ""
echo "Verify with:"
echo "  aws dynamodb get-item --profile $PROFILE --table-name $TABLE_NAME --key '{\"user_id\": {\"S\": \"$CLI_IAM_ARN\"}}'"
