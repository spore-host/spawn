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

# The region above is deliberately configurable — this stack is multi-region by
# design and the choice is echoed below. The ACCOUNT is not configurable, and was
# unchecked until #799 widened the deploy gates past scripts/deploy-*.sh, which
# this file's name had been escaping. Override only for a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

echo "Setting up spawn scheduled executions infrastructure..."
echo "  Account: $ACCOUNT_ID"
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

    # The two `|| true`s this replaces swallowed EVERY update-stack and wait
    # failure, and the script then printed "✅ Stack updated successfully"
    # unconditionally — a failed deploy reporting a green tick (#799).
    #
    # "No updates are to be performed" is the one failure that genuinely means
    # success, so it is matched explicitly and everything else aborts.
    set +e
    UPDATE_ERR=$(aws cloudformation update-stack \
        --stack-name "$STACK_NAME" \
        --template-body "file://${TEMPLATE_FILE}" \
        --parameters "ParameterKey=Environment,ParameterValue=${ENVIRONMENT}" \
        --region "$REGION" 2>&1 >/dev/null)
    UPDATE_RC=$?
    set -e

    if [ "$UPDATE_RC" -ne 0 ]; then
        if printf '%s' "$UPDATE_ERR" | grep -q "No updates are to be performed"; then
            echo "✅ Stack is already up to date — no changes to apply"
        else
            echo "ERROR: update-stack failed:" >&2
            printf '%s\n' "$UPDATE_ERR" >&2
            exit 1
        fi
    else
        echo "Waiting for stack update to complete..."
        aws cloudformation wait stack-update-complete \
            --stack-name "$STACK_NAME" \
            --region "$REGION"

        echo "✅ Stack updated successfully"
    fi
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
