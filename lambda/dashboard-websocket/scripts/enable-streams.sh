#!/bin/bash
set -euo pipefail

# Enable DynamoDB Streams on existing tables
# Usage: ./enable-streams.sh <aws-profile>

if [ $# -lt 1 ]; then
    echo "Usage: $0 <aws-profile>"
    echo "Example: $0 spore-host-infra"
    exit 1
fi

AWS_PROFILE=$1
REGION="us-east-1"

TABLES=(
    "spawn-sweep-orchestration"
    "spawn-autoscale-groups-production"
)

echo "Enabling DynamoDB Streams in region $REGION with profile $AWS_PROFILE"

for TABLE_NAME in "${TABLES[@]}"; do
    echo ""
    echo "Checking table: $TABLE_NAME"

    # Check if table exists
    if ! aws dynamodb describe-table --table-name "$TABLE_NAME" --region "$REGION" --profile "$AWS_PROFILE" >/dev/null 2>&1; then
        echo "Warning: Table $TABLE_NAME does not exist, skipping"
        continue
    fi

    # Check if stream is already enabled
    STREAM_STATUS=$(aws dynamodb describe-table \
        --table-name "$TABLE_NAME" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" \
        --query 'Table.StreamSpecification.StreamEnabled' \
        --output text 2>/dev/null || echo "None")

    if [ "$STREAM_STATUS" = "True" ]; then
        STREAM_ARN=$(aws dynamodb describe-table \
            --table-name "$TABLE_NAME" \
            --region "$REGION" \
            --profile "$AWS_PROFILE" \
            --query 'Table.LatestStreamArn' \
            --output text)
        echo "Stream already enabled for $TABLE_NAME"
        echo "Stream ARN: $STREAM_ARN"
    else
        echo "Enabling stream for $TABLE_NAME..."
        aws dynamodb update-table \
            --table-name "$TABLE_NAME" \
            --stream-specification StreamEnabled=true,StreamViewType=NEW_AND_OLD_IMAGES \
            --region "$REGION" \
            --profile "$AWS_PROFILE"

        # Wait for update to complete
        echo "Waiting for table update to complete..."
        aws dynamodb wait table-exists \
            --table-name "$TABLE_NAME" \
            --region "$REGION" \
            --profile "$AWS_PROFILE"

        STREAM_ARN=$(aws dynamodb describe-table \
            --table-name "$TABLE_NAME" \
            --region "$REGION" \
            --profile "$AWS_PROFILE" \
            --query 'Table.LatestStreamArn' \
            --output text)
        echo "Stream enabled for $TABLE_NAME"
        echo "Stream ARN: $STREAM_ARN"
    fi
done

echo ""
echo "All streams configured successfully"
