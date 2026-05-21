# Self-Hosted Infrastructure Guide

## Overview

Self-hosted mode allows you to run spawn infrastructure (Lambda, DynamoDB, S3) in your own AWS account instead of the shared spore-host-infra account.

**When you need this:**
- NIST 800-53 Moderate or High baseline compliance
- FedRAMP authorization (Moderate or High)
- Organizational policy requires customer-owned infrastructure
- Data residency requirements
- Enhanced security and isolation

**What gets deployed:**
- DynamoDB tables for schedules, sweeps, and alerts
- S3 buckets for binaries and schedule storage
- Lambda functions for orchestration
- CloudWatch Log Groups for audit logs
- IAM roles and policies

## Quick Start

### 1. Initialize Configuration

```bash
spawn config init --self-hosted
```

This interactive wizard will:
1. Prompt for AWS account ID
2. Prompt for regions to deploy
3. Generate `~/.spawn/config.yaml`
4. Provide CloudFormation deployment commands

### 2. Deploy CloudFormation Stack

```bash
cd deployment/cloudformation

aws cloudformation create-stack \
  --stack-name spawn-self-hosted \
  --template-body file://self-hosted-stack.yaml \
  --parameters \
      ParameterKey=NamePrefix,ParameterValue=my-spawn \
      ParameterKey=Regions,ParameterValue="us-east-1,us-west-2" \
  --capabilities CAPABILITY_IAM \
  --region us-east-1
```

### 3. Wait for Completion

```bash
aws cloudformation wait stack-create-complete \
  --stack-name spawn-self-hosted \
  --region us-east-1
```

Deployment time: ~5-10 minutes

### 4. Verify Deployment

```bash
spawn validate --infrastructure
```

Expected output:
```
Infrastructure Validation Report
=================================

Mode: Self-hosted infrastructure (customer account)

DynamoDB Tables:
  ✓ my-spawn-schedules
  ✓ my-spawn-sweep-orchestration
  ✓ my-spawn-alerts
  ✓ my-spawn-alert-history
  (4/4 accessible)

S3 Buckets:
  ✓ my-spawn-binaries-us-east-1
  ✓ my-spawn-schedules-us-east-1
  (2/2 accessible)

Lambda Functions:
  ✓ my-spawn-scheduler-handler
  ✓ my-spawn-sweep-orchestrator
  ✓ my-spawn-alert-handler
  (3/3 accessible)

Status: ✓ All resources accessible
```

### 5. Launch Instances

```bash
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=moderate \
  --ttl 4h
```

spawn automatically uses your self-hosted infrastructure.

## Manual Deployment (Without CloudFormation)

If you can't use CloudFormation, deploy resources manually:

### 1. DynamoDB Tables

```bash
# Schedules table
aws dynamodb create-table \
  --table-name my-spawn-schedules \
  --attribute-definitions \
      AttributeName=schedule_id,AttributeType=S \
  --key-schema \
      AttributeName=schedule_id,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST \
  --region us-east-1

# Sweep orchestration table
aws dynamodb create-table \
  --table-name my-spawn-sweep-orchestration \
  --attribute-definitions \
      AttributeName=sweep_id,AttributeType=S \
  --key-schema \
      AttributeName=sweep_id,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST \
  --region us-east-1

# Alerts table
aws dynamodb create-table \
  --table-name my-spawn-alerts \
  --attribute-definitions \
      AttributeName=alert_id,AttributeType=S \
  --key-schema \
      AttributeName=alert_id,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST \
  --region us-east-1

# Alert history table
aws dynamodb create-table \
  --table-name my-spawn-alert-history \
  --attribute-definitions \
      AttributeName=alert_id,AttributeType=S \
      AttributeName=timestamp,AttributeType=N \
  --key-schema \
      AttributeName=alert_id,KeyType=HASH \
      AttributeName=timestamp,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST \
  --region us-east-1
```

### 2. S3 Buckets

```bash
# Binaries bucket (per region)
aws s3 mb s3://my-spawn-binaries-us-east-1 --region us-east-1

# Enable encryption
aws s3api put-bucket-encryption \
  --bucket my-spawn-binaries-us-east-1 \
  --server-side-encryption-configuration '{
    "Rules": [{
      "ApplyServerSideEncryptionByDefault": {
        "SSEAlgorithm": "AES256"
      }
    }]
  }'

# Block public access
aws s3api put-public-access-block \
  --bucket my-spawn-binaries-us-east-1 \
  --public-access-block-configuration \
      "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"

# Schedules bucket (per region)
aws s3 mb s3://my-spawn-schedules-us-east-1 --region us-east-1
# Repeat encryption and public access block
```

### 3. Lambda Functions

```bash
# Build Lambda packages
cd lambda/scheduler-handler
GOOS=linux GOARCH=amd64 go build -o bootstrap
zip scheduler-handler.zip bootstrap

# Create IAM role
aws iam create-role \
  --role-name spawn-lambda-role \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": {"Service": "lambda.amazonaws.com"},
      "Action": "sts:AssumeRole"
    }]
  }'

# Attach policies
aws iam attach-role-policy \
  --role-name spawn-lambda-role \
  --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole

aws iam attach-role-policy \
  --role-name spawn-lambda-role \
  --policy-arn arn:aws:iam::aws:policy/AmazonDynamoDBFullAccess

# Create function
aws lambda create-function \
  --function-name my-spawn-scheduler-handler \
  --runtime provided.al2 \
  --role arn:aws:iam::123456789012:role/spawn-lambda-role \
  --handler bootstrap \
  --zip-file fileb://scheduler-handler.zip \
  --timeout 300 \
  --memory-size 256 \
  --environment Variables='{
    "SPAWN_ACCOUNT_ID":"123456789012",
    "SPAWN_SCHEDULES_TABLE":"my-spawn-schedules",
    "SPAWN_SCHEDULE_HISTORY_TABLE":"my-spawn-schedule-history",
    "SPAWN_SCHEDULES_BUCKET_TEMPLATE":"my-spawn-schedules-%s",
    "SPAWN_ORCHESTRATOR_FUNCTION_NAME":"my-spawn-sweep-orchestrator"
  }'
```

Repeat for sweep-orchestrator, alert-handler, and dashboard-api Lambda functions.

## Configuration

Update `~/.spawn/config.yaml` with your resource names:

```yaml
infrastructure:
  mode: self-hosted

  # DynamoDB tables
  dynamodb:
    schedules_table: my-spawn-schedules
    sweep_orchestration_table: my-spawn-sweep-orchestration
    alerts_table: my-spawn-alerts
    alert_history_table: my-spawn-alert-history

  # S3 buckets (region suffix added automatically)
  s3:
    binaries_bucket_prefix: my-spawn-binaries
    schedules_bucket_prefix: my-spawn-schedules

  # Lambda functions
  lambda:
    scheduler_handler_arn: arn:aws:lambda:us-east-1:123456789012:function:my-spawn-scheduler-handler
    sweep_orchestrator_arn: arn:aws:lambda:us-east-1:123456789012:function:my-spawn-sweep-orchestrator
    alert_handler_arn: arn:aws:lambda:us-east-1:123456789012:function:my-spawn-alert-handler
    dashboard_api_arn: arn:aws:lambda:us-east-1:123456789012:function:my-spawn-dashboard-api
```

### Environment Variable Override

For CI/CD or temporary testing:

```bash
export SPAWN_INFRASTRUCTURE_MODE=self-hosted
export SPAWN_DYNAMODB_SCHEDULES_TABLE=my-spawn-schedules
export SPAWN_S3_BINARIES_BUCKET_PREFIX=my-spawn-binaries
export SPAWN_LAMBDA_SCHEDULER_ARN=arn:aws:lambda:...
```

## Cost Estimation

Typical monthly costs for self-hosted infrastructure:

### DynamoDB (On-Demand)
- Light usage (<1M requests/month): $1-3
- Moderate usage (<10M requests/month): $5-10
- Heavy usage (>10M requests/month): $10-50

**Recommendation:** Use on-demand pricing for simplicity.

### S3
- Storage (binaries + schedules): $0.50-2/month
- Requests: $0.10-1/month
- **Total:** ~$1-3/month

### Lambda
- Invocations: Free tier covers typical usage
- Compute time: $0.20-2/month
- **Total:** ~$0-2/month (usually free tier)

### CloudWatch Logs
- Log storage: $0.50-2/month
- Log ingestion: $0.50-1/month
- **Total:** ~$1-3/month

### Total Estimated Cost
**$3-20/month** for typical usage (10-100 instances/day)

Compare to shared infrastructure: $0/month (included with spawn)

## Multi-Region Deployment

Deploy to multiple regions for redundancy:

### 1. Deploy Stack to Each Region

```bash
for region in us-east-1 us-west-2 eu-west-1; do
  aws cloudformation create-stack \
    --stack-name spawn-self-hosted \
    --template-body file://self-hosted-stack.yaml \
    --parameters ParameterKey=NamePrefix,ParameterValue=my-spawn \
    --capabilities CAPABILITY_IAM \
    --region $region
done
```

### 2. Update Config for Multi-Region

```yaml
infrastructure:
  mode: self-hosted
  
  # Tables are regional (automatically resolved)
  dynamodb:
    schedules_table: my-spawn-schedules
  
  # Buckets are regional (suffix added automatically)
  s3:
    binaries_bucket_prefix: my-spawn-binaries
  
  # Lambda ARNs per region
  lambda:
    scheduler_handler_arn_template: "arn:aws:lambda:{region}:123456789012:function:my-spawn-scheduler-handler"
```

## Migration from Shared to Self-Hosted

### Pre-Migration Checklist

- [ ] Backup existing schedules and sweep configurations
- [ ] Document current instance configurations
- [ ] Test CloudFormation deployment in dev account
- [ ] Estimate costs for self-hosted infrastructure
- [ ] Plan maintenance window (if needed)

### Migration Steps

#### 1. Deploy Self-Hosted Infrastructure (No Downtime)

```bash
# Deploy without switching
aws cloudformation create-stack \
  --stack-name spawn-self-hosted \
  --template-body file://self-hosted-stack.yaml \
  --capabilities CAPABILITY_IAM
```

#### 2. Validate Deployment

```bash
spawn validate --infrastructure
```

#### 3. Switch Configuration

```bash
# Backup current config
cp ~/.spawn/config.yaml ~/.spawn/config.yaml.backup

# Update to self-hosted
cat > ~/.spawn/config.yaml <<EOF
infrastructure:
  mode: self-hosted
  # ... (add resource names from stack outputs)
EOF
```

#### 4. Test Launch

```bash
# Test launch in non-production
spawn launch \
  --instance-type t3.micro \
  --ttl 5m \
  --region us-east-1
```

#### 5. Migrate Schedules (Optional)

```bash
# Export schedules from shared infrastructure
spawn schedule list --output json > schedules-backup.json

# Import to self-hosted infrastructure
# (Manual process - schedules are in DynamoDB)
```

#### 6. Verify All Features

- [ ] Instance launch/termination
- [ ] Scheduled launches
- [ ] Parameter sweeps
- [ ] Alerts
- [ ] Validation commands

### Rollback Plan

If issues arise:

```bash
# Restore original config
cp ~/.spawn/config.yaml.backup ~/.spawn/config.yaml

# Verify shared infrastructure still works
spawn launch --instance-type t3.micro --ttl 5m
```

## Troubleshooting

### Issue: "DynamoDB table not found"

**Problem:** Table doesn't exist or wrong name configured.

**Solution:**
```bash
# Verify table exists
aws dynamodb describe-table --table-name my-spawn-schedules

# Check config has correct table name
grep schedules_table ~/.spawn/config.yaml
```

### Issue: "S3 bucket access denied"

**Problem:** Bucket doesn't exist or IAM permissions missing.

**Solution:**
```bash
# Verify bucket exists
aws s3 ls s3://my-spawn-binaries-us-east-1

# Check IAM role has S3 permissions
aws iam get-role-policy --role-name spawn-role --policy-name s3-access
```

### Issue: "Lambda function not found"

**Problem:** Function ARN incorrect or doesn't exist.

**Solution:**
```bash
# Verify function exists
aws lambda get-function --function-name my-spawn-scheduler-handler

# Check ARN format in config
grep scheduler_handler_arn ~/.spawn/config.yaml
```

### Issue: "High costs"

**Problem:** DynamoDB provisioned throughput or excessive requests.

**Solution:**
```bash
# Switch to on-demand pricing
aws dynamodb update-table \
  --table-name my-spawn-schedules \
  --billing-mode PAY_PER_REQUEST

# Review CloudWatch metrics for usage patterns
aws cloudwatch get-metric-statistics \
  --namespace AWS/DynamoDB \
  --metric-name ConsumedReadCapacityUnits \
  --dimensions Name=TableName,Value=my-spawn-schedules \
  --start-time 2026-01-20T00:00:00Z \
  --end-time 2026-01-27T00:00:00Z \
  --period 3600 \
  --statistics Sum
```

## Security Best Practices

### 1. Least Privilege IAM Policies

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:Query",
        "dynamodb:Scan",
        "dynamodb:UpdateItem",
        "dynamodb:DeleteItem"
      ],
      "Resource": "arn:aws:dynamodb:us-east-1:123456789012:table/my-spawn-*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::my-spawn-*",
        "arn:aws:s3:::my-spawn-*/*"
      ]
    }
  ]
}
```

### 2. Enable CloudTrail Logging

```bash
aws cloudtrail create-trail \
  --name spawn-infrastructure-trail \
  --s3-bucket-name my-cloudtrail-logs \
  --include-global-service-events
```

### 3. Enable VPC Endpoints (High Baseline)

```bash
# DynamoDB VPC endpoint
aws ec2 create-vpc-endpoint \
  --vpc-id $VPC_ID \
  --service-name com.amazonaws.us-east-1.dynamodb \
  --route-table-ids $ROUTE_TABLE_ID

# S3 VPC endpoint
aws ec2 create-vpc-endpoint \
  --vpc-id $VPC_ID \
  --service-name com.amazonaws.us-east-1.s3 \
  --route-table-ids $ROUTE_TABLE_ID
```

### 4. Enable Encryption at Rest

```bash
# DynamoDB encryption
aws dynamodb update-table \
  --table-name my-spawn-schedules \
  --sse-specification Enabled=true,SSEType=KMS,KMSMasterKeyId=alias/my-key

# S3 encryption (with customer KMS)
aws s3api put-bucket-encryption \
  --bucket my-spawn-binaries-us-east-1 \
  --server-side-encryption-configuration '{
    "Rules": [{
      "ApplyServerSideEncryptionByDefault": {
        "SSEAlgorithm": "aws:kms",
        "KMSMasterKeyID": "arn:aws:kms:us-east-1:123456789012:key/..."
      }
    }]
  }'
```

## Monitoring and Alerts

### CloudWatch Dashboards

```bash
# Create dashboard for spawn infrastructure
aws cloudwatch put-dashboard \
  --dashboard-name spawn-infrastructure \
  --dashboard-body file://dashboard.json
```

### CloudWatch Alarms

```bash
# DynamoDB throttling alarm
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-dynamodb-throttled \
  --metric-name UserErrors \
  --namespace AWS/DynamoDB \
  --statistic Sum \
  --period 300 \
  --evaluation-periods 1 \
  --threshold 10 \
  --comparison-operator GreaterThanThreshold

# Lambda errors alarm
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-lambda-errors \
  --metric-name Errors \
  --namespace AWS/Lambda \
  --statistic Sum \
  --period 300 \
  --evaluation-periods 1 \
  --threshold 5 \
  --comparison-operator GreaterThanThreshold
```

## Additional Resources

- [CloudFormation Template Reference](../../deployment/cloudformation/README.md)
- [NIST 800-53 Baselines Guide](../compliance/nist-800-53-baselines.md)
- [Compliance Validation Guide](compliance-validation.md)
- [Cost Optimization Guide](cost-optimization.md)

## Support

- GitHub Issues: https://github.com/spore-host/spore-host/issues
- Infrastructure validation: `spawn validate --infrastructure`
- Config help: `spawn config --help`
