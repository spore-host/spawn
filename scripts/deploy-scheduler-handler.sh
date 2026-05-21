#!/bin/bash
set -e

# Script to deploy the scheduler-handler Lambda function

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LAMBDA_DIR="${SCRIPT_DIR}/../lambda/scheduler-handler"
FUNCTION_NAME="${SPAWN_LAMBDA_NAME:-scheduler-handler}"
REGION="${AWS_REGION:-us-east-1}"
PROFILE="${AWS_PROFILE:-spore-host-infra}"

usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -r, --region REGION       AWS region (default: us-east-1)"
    echo "  -p, --profile PROFILE     AWS profile (default: spore-host-infra)"
    echo "  -f, --function NAME       Lambda function name (default: scheduler-handler)"
    echo "  -h, --help                Show this help message"
    echo ""
    echo "Environment variables:"
    echo "  AWS_REGION                AWS region"
    echo "  AWS_PROFILE               AWS profile"
    echo "  SPAWN_LAMBDA_NAME         Lambda function name"
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -r|--region)
            REGION="$2"
            shift 2
            ;;
        -p|--profile)
            PROFILE="$2"
            shift 2
            ;;
        -f|--function)
            FUNCTION_NAME="$2"
            shift 2
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo "Unknown option: $1"
            usage
            ;;
    esac
done

echo "Deploying scheduler-handler Lambda function..."
echo "  Region: $REGION"
echo "  Profile: $PROFILE"
echo "  Function: $FUNCTION_NAME"
echo ""

# Navigate to Lambda directory
cd "$LAMBDA_DIR"

# Install dependencies
echo "📦 Installing dependencies..."
go mod download

# Build Lambda function
echo "🔨 Building Lambda function..."
GOOS=linux GOARCH=amd64 go build -tags lambda.norpc -o bootstrap main.go

# Create deployment package
echo "📦 Creating deployment package..."
zip -q function.zip bootstrap
rm bootstrap

# Check if function exists
if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$PROFILE" &>/dev/null; then
    echo "📤 Updating existing Lambda function..."
    aws lambda update-function-code \
        --function-name "$FUNCTION_NAME" \
        --zip-file fileb://function.zip \
        --region "$REGION" \
        --profile "$PROFILE" \
        --output table

    echo ""
    echo "⏳ Waiting for update to complete..."
    aws lambda wait function-updated \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" \
        --profile "$PROFILE"

    echo "✅ Lambda function updated successfully"
else
    echo "🆕 Creating new Lambda function..."

    # Get account ID
    ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text --profile "$PROFILE")
    ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/SpawnSchedulerHandlerExecutionRole"

    aws lambda create-function \
        --function-name "$FUNCTION_NAME" \
        --runtime provided.al2 \
        --role "$ROLE_ARN" \
        --handler bootstrap \
        --zip-file fileb://function.zip \
        --timeout 300 \
        --memory-size 512 \
        --region "$REGION" \
        --profile "$PROFILE" \
        --environment "Variables={}" \
        --description "Handles EventBridge Scheduler triggers for spawn scheduled executions" \
        --tags "Application=spawn,Component=scheduler" \
        --output table

    echo "✅ Lambda function created successfully"
fi

# Update configuration if needed
echo ""
echo "⚙️  Updating Lambda configuration..."
aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --timeout 300 \
    --memory-size 512 \
    --region "$REGION" \
    --profile "$PROFILE" \
    --output table &>/dev/null || true

echo ""
echo "✅ Deployment complete!"
echo ""
echo "Function ARN:"
aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" --profile "$PROFILE" --query 'Configuration.FunctionArn' --output text
echo ""
