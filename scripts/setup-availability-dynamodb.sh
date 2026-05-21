#!/bin/bash
set -e

REGION="${AWS_REGION:-us-east-1}"
PROFILE="${AWS_PROFILE:-spore-host-infra}"
TABLE_NAME="spawn-availability-stats"

echo "Creating DynamoDB table: $TABLE_NAME in $REGION (profile: $PROFILE)"

aws dynamodb create-table \
  --profile "$PROFILE" \
  --table-name "$TABLE_NAME" \
  --region "$REGION" \
  --attribute-definitions \
    AttributeName=stat_id,AttributeType=S \
  --key-schema \
    AttributeName=stat_id,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST \
  --tags \
    Key=spawn:managed,Value=true \
    Key=spawn:purpose,Value=availability-stats

echo "Waiting for table to be active..."
aws dynamodb wait table-exists --profile "$PROFILE" --table-name "$TABLE_NAME" --region "$REGION"

echo "Enabling point-in-time recovery..."
aws dynamodb update-continuous-backups \
  --profile "$PROFILE" \
  --table-name "$TABLE_NAME" \
  --region "$REGION" \
  --point-in-time-recovery-specification PointInTimeRecoveryEnabled=true

echo "Enabling TTL (7-day expiration on ttl_timestamp field)..."
aws dynamodb update-time-to-live \
  --profile "$PROFILE" \
  --table-name "$TABLE_NAME" \
  --region "$REGION" \
  --time-to-live-specification "Enabled=true, AttributeName=ttl_timestamp"

echo ""
echo "✅ Table $TABLE_NAME created successfully"
echo "   Region: $REGION"
echo "   Billing: PAY_PER_REQUEST"
echo "   TTL: 7 days (ttl_timestamp field)"
echo ""
