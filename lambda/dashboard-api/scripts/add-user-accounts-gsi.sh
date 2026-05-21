#!/bin/bash
set -e

PROFILE=${1:-spore-host-infra}
TABLE_NAME="spawn-user-accounts"

echo "Adding GSIs to $TABLE_NAME..."

aws dynamodb update-table \
  --profile "$PROFILE" \
  --region us-east-1 \
  --table-name "$TABLE_NAME" \
  --attribute-definitions \
    AttributeName=email,AttributeType=S \
    AttributeName=cli_iam_arn,AttributeType=S \
  --global-secondary-index-updates '[
    {
      "Create": {
        "IndexName": "email-index",
        "KeySchema": [{"AttributeName": "email", "KeyType": "HASH"}],
        "Projection": {"ProjectionType": "ALL"},
        "ProvisionedThroughput": {"ReadCapacityUnits": 5, "WriteCapacityUnits": 5}
      }
    },
    {
      "Create": {
        "IndexName": "cli_iam_arn-index",
        "KeySchema": [{"AttributeName": "cli_iam_arn", "KeyType": "HASH"}],
        "Projection": {"ProjectionType": "ALL"},
        "ProvisionedThroughput": {"ReadCapacityUnits": 5, "WriteCapacityUnits": 5}
      }
    }
  ]'

echo "✓ GSIs created. Status: CREATING (will take 5-10 minutes to become ACTIVE)"
echo ""
echo "Check status with:"
echo "  aws dynamodb describe-table --profile $PROFILE --table-name $TABLE_NAME --query 'Table.GlobalSecondaryIndexes[*].[IndexName,IndexStatus]' --output table"
