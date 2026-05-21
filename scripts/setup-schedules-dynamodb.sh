#!/bin/bash
set -e

# Script to create DynamoDB tables for spawn scheduled executions

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE_FILE="${SCRIPT_DIR}/../deployment/cloudformation/schedules-tables.yaml"
STACK_NAME="${SPAWN_STACK_NAME:-spawn-schedules}"
REGION="${AWS_REGION:-us-east-1}"
ENVIRONMENT="${SPAWN_ENVIRONMENT:-production}"

usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  -r, --region REGION       AWS region (default: us-east-1)"
    echo "  -e, --environment ENV     Environment (production|staging|development, default: production)"
    echo "  -s, --stack-name NAME     CloudFormation stack name (default: spawn-schedules)"
    echo "  -h, --help                Show this help message"
    echo ""
    echo "Environment variables:"
    echo "  AWS_REGION                AWS region"
    echo "  SPAWN_ENVIRONMENT         Environment name"
    echo "  SPAWN_STACK_NAME          CloudFormation stack name"
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -r|--region)
            REGION="$2"
            shift 2
            ;;
        -e|--environment)
            ENVIRONMENT="$2"
            shift 2
            ;;
        -s|--stack-name)
            STACK_NAME="$2"
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

echo "Setting up spawn scheduled executions infrastructure..."
echo "  Region: $REGION"
echo "  Environment: $ENVIRONMENT"
echo "  Stack: $STACK_NAME"
echo ""

if ! aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$REGION" &>/dev/null; then
    echo "Creating CloudFormation stack..."
    aws cloudformation create-stack \
        --stack-name "$STACK_NAME" \
        --template-body "file://${TEMPLATE_FILE}" \
        --parameters "ParameterKey=Environment,ParameterValue=${ENVIRONMENT}" \
        --region "$REGION" \
        --tags "Key=Application,Value=spawn" "Key=Component,Value=scheduler"

    echo "Waiting for stack creation to complete..."
    aws cloudformation wait stack-create-complete \
        --stack-name "$STACK_NAME" \
        --region "$REGION"

    echo "✅ Stack created successfully"
else
    echo "Stack already exists. Updating..."
    aws cloudformation update-stack \
        --stack-name "$STACK_NAME" \
        --template-body "file://${TEMPLATE_FILE}" \
        --parameters "ParameterKey=Environment,ParameterValue=${ENVIRONMENT}" \
        --region "$REGION" 2>&1 | grep -v "No updates are to be performed" || true

    echo "Waiting for stack update to complete..."
    aws cloudformation wait stack-update-complete \
        --stack-name "$STACK_NAME" \
        --region "$REGION" 2>/dev/null || true

    echo "✅ Stack updated successfully"
fi

echo ""
echo "DynamoDB tables created:"
aws cloudformation describe-stacks \
    --stack-name "$STACK_NAME" \
    --region "$REGION" \
    --query 'Stacks[0].Outputs' \
    --output table

echo ""
echo "✅ Setup complete!"
