#!/bin/bash
set -e

TABLE_NAME="spawn-sweep-orchestration"
REGION="us-east-1"

echo "Creating DynamoDB table for sweep orchestration..."

# Create table
aws dynamodb create-table \
  --table-name "$TABLE_NAME" \
  --region "$REGION" \
  --attribute-definitions \
    AttributeName=sweep_id,AttributeType=S \
    AttributeName=user_id,AttributeType=S \
    AttributeName=created_at,AttributeType=S \
  --key-schema \
    AttributeName=sweep_id,KeyType=HASH \
  --global-secondary-indexes '[{
    "IndexName": "user_id-created_at-index",
    "KeySchema": [
      {"AttributeName": "user_id", "KeyType": "HASH"},
      {"AttributeName": "created_at", "KeyType": "RANGE"}
    ],
    "Projection": {"ProjectionType": "ALL"},
    "ProvisionedThroughput": {
      "ReadCapacityUnits": 5,
      "WriteCapacityUnits": 5
    }
  }]' \
  --billing-mode PROVISIONED \
  --provisioned-throughput ReadCapacityUnits=5,WriteCapacityUnits=5 \
  2>/dev/null || echo "Table already exists"

# Wait for table to be active
echo "Waiting for table to be active..."
aws dynamodb wait table-exists --table-name "$TABLE_NAME" --region "$REGION"

# Update to PAY_PER_REQUEST for cost optimization
aws dynamodb update-table \
  --table-name "$TABLE_NAME" \
  --region "$REGION" \
  --billing-mode PAY_PER_REQUEST 2>/dev/null || true

# Enable point-in-time recovery
aws dynamodb update-continuous-backups \
  --table-name "$TABLE_NAME" \
  --region "$REGION" \
  --point-in-time-recovery-specification PointInTimeRecoveryEnabled=true 2>/dev/null || true

echo "✅ DynamoDB table $TABLE_NAME configured successfully"
