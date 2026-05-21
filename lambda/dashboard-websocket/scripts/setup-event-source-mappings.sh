#!/bin/bash
set -euo pipefail

# Setup event source mappings from DynamoDB Streams to processor Lambda
# Usage: ./setup-event-source-mappings.sh <aws-profile>

if [ $# -lt 1 ]; then
    echo "Usage: $0 <aws-profile>"
    echo "Example: $0 spore-host-infra"
    exit 1
fi

AWS_PROFILE=$1
REGION="us-east-1"
FUNCTION_NAME="spawn-dashboard-websocket-processor"

TABLES=(
    "spawn-sweep-orchestration"
    "spawn-autoscale-groups-production"
)

echo "Setting up event source mappings for $FUNCTION_NAME in region $REGION with profile $AWS_PROFILE"

# Get Lambda function ARN
LAMBDA_ARN=$(aws lambda get-function \
    --function-name "$FUNCTION_NAME" \
    --region "$REGION" \
    --profile "$AWS_PROFILE" \
    --query 'Configuration.FunctionArn' \
    --output text)

echo "Lambda ARN: $LAMBDA_ARN"

for TABLE_NAME in "${TABLES[@]}"; do
    echo ""
    echo "Setting up event source for table: $TABLE_NAME"

    # Get stream ARN
    STREAM_ARN=$(aws dynamodb describe-table \
        --table-name "$TABLE_NAME" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" \
        --query 'Table.LatestStreamArn' \
        --output text 2>/dev/null || echo "None")

    if [ "$STREAM_ARN" = "None" ] || [ -z "$STREAM_ARN" ]; then
        echo "Warning: No stream found for $TABLE_NAME. Run enable-streams.sh first."
        continue
    fi

    echo "Stream ARN: $STREAM_ARN"

    # Check if event source mapping already exists
    EXISTING_MAPPING=$(aws lambda list-event-source-mappings \
        --function-name "$FUNCTION_NAME" \
        --event-source-arn "$STREAM_ARN" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" \
        --query 'EventSourceMappings[0].UUID' \
        --output text 2>/dev/null || echo "None")

    if [ "$EXISTING_MAPPING" != "None" ] && [ -n "$EXISTING_MAPPING" ]; then
        echo "Event source mapping already exists for $TABLE_NAME: $EXISTING_MAPPING"
    else
        echo "Creating event source mapping for $TABLE_NAME..."
        UUID=$(aws lambda create-event-source-mapping \
            --function-name "$FUNCTION_NAME" \
            --event-source-arn "$STREAM_ARN" \
            --starting-position LATEST \
            --batch-size 10 \
            --maximum-batching-window-in-seconds 1 \
            --region "$REGION" \
            --profile "$AWS_PROFILE" \
            --query 'UUID' \
            --output text)
        echo "Created event source mapping for $TABLE_NAME: $UUID"
    fi
done

echo ""
echo "Event source mappings configured successfully!"
echo ""
echo "To verify mappings, run:"
echo "aws lambda list-event-source-mappings --function-name $FUNCTION_NAME --region $REGION --profile $AWS_PROFILE"
