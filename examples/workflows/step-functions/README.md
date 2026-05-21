# AWS Step Functions Integration Example

## Overview

AWS Step Functions provides serverless workflow orchestration with visual monitoring and built-in error handling.

## Prerequisites

```bash
# AWS CLI
aws --version

# Ensure spawn is available in Lambda layer or container
```

## Setup

1. Package Lambda functions:
```bash
cd lambda
zip launcher.zip launcher.py
zip status_checker.zip status_checker.py
```

2. Create Lambda layer with spawn binary (or use Docker container)

3. Deploy Lambda functions:
```bash
aws lambda create-function \
    --function-name spawn-launcher \
    --runtime python3.11 \
    --handler launcher.handler \
    --zip-file fileb://launcher.zip \
    --role arn:aws:iam::ACCOUNT:role/lambda-execution-role

aws lambda create-function \
    --function-name spawn-status-checker \
    --runtime python3.11 \
    --handler status_checker.handler \
    --zip-file fileb://status_checker.zip \
    --role arn:aws:iam::ACCOUNT:role/lambda-execution-role
```

4. Create state machine:
```bash
aws stepfunctions create-state-machine \
    --name spawn-parameter-sweep \
    --definition file://sweep_statemachine.json \
    --role-arn arn:aws:iam::ACCOUNT:role/stepfunctions-execution-role
```

## Running

```bash
# Start execution
aws stepfunctions start-execution \
    --state-machine-arn arn:aws:states:us-east-1:ACCOUNT:stateMachine:spawn-parameter-sweep \
    --input '{"params_file": "/config/sweep.yaml"}'

# Monitor execution
aws stepfunctions describe-execution \
    --execution-arn arn:aws:states:us-east-1:ACCOUNT:execution:spawn-parameter-sweep:exec-id
```

## See Also

- [Step Functions Documentation](https://docs.aws.amazon.com/step-functions/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
