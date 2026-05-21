#!/bin/bash
set -e

PROFILE=${1:-spore-host-infra}
TABLE_NAME="spawn-autoscale-groups-production"

echo "Adding user_id GSI to $TABLE_NAME..."

aws dynamodb update-table \
  --profile "$PROFILE" \
  --region us-east-1 \
  --table-name "$TABLE_NAME" \
  --attribute-definitions \
    AttributeName=user_id,AttributeType=S \
  --global-secondary-index-updates '[
    {
      "Create": {
        "IndexName": "user_id-index",
        "KeySchema": [{"AttributeName": "user_id", "KeyType": "HASH"}],
        "Projection": {"ProjectionType": "ALL"},
        "ProvisionedThroughput": {"ReadCapacityUnits": 5, "WriteCapacityUnits": 5}
      }
    }
  ]'

echo "✓ user_id-index created. Status: CREATING (will take 5-10 minutes to become ACTIVE)"
echo ""
echo "Check status with:"
echo "  aws dynamodb describe-table --profile $PROFILE --table-name $TABLE_NAME --query 'Table.GlobalSecondaryIndexes[*].[IndexName,IndexStatus]' --output table"
