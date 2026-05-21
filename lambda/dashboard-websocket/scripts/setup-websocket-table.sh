#!/bin/bash
set -euo pipefail

# Setup spawn-websocket-connections DynamoDB table
# Usage: ./setup-websocket-table.sh <aws-profile>

if [ $# -lt 1 ]; then
    echo "Usage: $0 <aws-profile>"
    echo "Example: $0 spore-host-infra"
    exit 1
fi

AWS_PROFILE=$1
TABLE_NAME="spawn-websocket-connections"
REGION="us-east-1"

echo "Setting up $TABLE_NAME table in region $REGION with profile $AWS_PROFILE"

# Check if table already exists
if aws dynamodb describe-table --table-name "$TABLE_NAME" --region "$REGION" --profile "$AWS_PROFILE" >/dev/null 2>&1; then
    echo "Table $TABLE_NAME already exists"

    # Check if TTL is enabled
    TTL_STATUS=$(aws dynamodb describe-time-to-live \
        --table-name "$TABLE_NAME" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" \
        --query 'TimeToLiveDescription.TimeToLiveStatus' \
        --output text)

    if [ "$TTL_STATUS" != "ENABLED" ]; then
        echo "Enabling TTL on $TABLE_NAME..."
        aws dynamodb update-time-to-live \
            --table-name "$TABLE_NAME" \
            --time-to-live-specification "Enabled=true,AttributeName=ttl" \
            --region "$REGION" \
            --profile "$AWS_PROFILE"
        echo "TTL enabled"
    else
        echo "TTL already enabled"
    fi

    exit 0
fi

# Create the table
echo "Creating table $TABLE_NAME..."
aws dynamodb create-table \
    --table-name "$TABLE_NAME" \
    --attribute-definitions \
        AttributeName=connection_id,AttributeType=S \
        AttributeName=user_id,AttributeType=S \
    --key-schema \
        AttributeName=connection_id,KeyType=HASH \
    --global-secondary-indexes \
        "IndexName=user_id-index,KeySchema=[{AttributeName=user_id,KeyType=HASH}],Projection={ProjectionType=ALL},ProvisionedThroughput={ReadCapacityUnits=5,WriteCapacityUnits=5}" \
    --provisioned-throughput \
        ReadCapacityUnits=5,WriteCapacityUnits=5 \
    --tags \
        Key=spawn:managed,Value=true \
        Key=spawn:purpose,Value=websocket \
    --region "$REGION" \
    --profile "$AWS_PROFILE"

echo "Waiting for table to become active..."
aws dynamodb wait table-exists \
    --table-name "$TABLE_NAME" \
    --region "$REGION" \
    --profile "$AWS_PROFILE"

# Enable TTL
echo "Enabling TTL on $TABLE_NAME..."
aws dynamodb update-time-to-live \
    --table-name "$TABLE_NAME" \
    --time-to-live-specification "Enabled=true,AttributeName=ttl" \
    --region "$REGION" \
    --profile "$AWS_PROFILE"

echo "Table $TABLE_NAME created successfully with TTL enabled"
echo "GSI user_id-index created for querying connections by user"
