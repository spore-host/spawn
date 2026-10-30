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

# The tables whose streams feed the processor. Keep in step with the table
# switch in lambda/dashboard-websocket-processor/main.go, ProcessorStreams in
# lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh and
# enable-streams.sh; dashboard_streams_test.go checks that they agree.
TABLES=(
    "spawn-sweep-orchestration"
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

# Named deleter (#1434): remove any DynamoDB-stream mapping on the processor
# whose source table no longer exists. A mapping outlives its table, sitting
# Disabled with "Stream is closed" (the removed autoscale table left one).
# Only a ResourceNotFoundException from describe-table counts as gone; any
# other failure keeps the mapping, so an existing table's mapping is never
# deleted. Mappings from other sources (SQS, Kinesis) are left alone.
echo ""
echo "Checking existing event source mappings for deleted source tables..."
MAPPINGS=$(aws lambda list-event-source-mappings \
    --function-name "$FUNCTION_NAME" \
    --region "$REGION" \
    --profile "$AWS_PROFILE" \
    --query 'EventSourceMappings[].[UUID,EventSourceArn]' \
    --output text)
REMOVED=0
while IFS=$'\t' read -r MAP_UUID SOURCE_ARN; do
    case "$SOURCE_ARN" in
        arn:aws:dynamodb:*:table/*/stream/*) ;;
        *) continue ;;
    esac
    SOURCE_TABLE=${SOURCE_ARN#*:table/}
    SOURCE_TABLE=${SOURCE_TABLE%%/*}
    if DESCRIBE_ERR=$(aws dynamodb describe-table \
        --table-name "$SOURCE_TABLE" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" 2>&1 >/dev/null); then
        continue
    fi
    case "$DESCRIBE_ERR" in
        *ResourceNotFoundException*) ;;
        *)
            echo "Warning: could not check table $SOURCE_TABLE, keeping mapping $MAP_UUID: $DESCRIBE_ERR"
            continue ;;
    esac
    aws lambda delete-event-source-mapping \
        --uuid "$MAP_UUID" \
        --region "$REGION" \
        --profile "$AWS_PROFILE" >/dev/null
    echo "Removed event source mapping $MAP_UUID: table $SOURCE_TABLE no longer exists ($SOURCE_ARN)"
    REMOVED=$((REMOVED + 1))
done <<< "$MAPPINGS"
echo "Removed $REMOVED event source mapping(s) whose source table no longer exists."

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
