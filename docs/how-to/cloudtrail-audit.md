# CloudTrail Audit Logging

Enable comprehensive audit logging for spawn operations using AWS CloudTrail.

## Overview

AWS CloudTrail provides audit trails for all AWS API calls made by spawn, including:
- Instance launches and terminations
- IAM role creation and modification
- Security group creation
- DynamoDB state updates
- S3 artifact access
- Lambda function invocations

Combined with spawn's built-in audit logging (`pkg/audit`), CloudTrail provides complete accountability for all spawn operations.

---

## Prerequisites

- AWS account with CloudTrail permissions
- S3 bucket for CloudTrail logs
- (Optional) CloudWatch Logs for real-time monitoring

---

## Quick Start

### 1. Create CloudTrail Trail

```bash
# Create S3 bucket for logs
aws s3 mb s3://my-spawn-audit-logs --region us-east-1

# Create CloudTrail trail
aws cloudtrail create-trail \
  --name spawn-audit \
  --s3-bucket-name my-spawn-audit-logs \
  --is-multi-region-trail \
  --enable-log-file-validation

# Start logging
aws cloudtrail start-logging --name spawn-audit

# Verify
aws cloudtrail get-trail-status --name spawn-audit
```

### 2. Enable CloudWatch Logs Integration (Optional)

```bash
# Create CloudWatch Logs log group
aws logs create-log-group --log-group-name /aws/cloudtrail/spawn

# Create IAM role for CloudTrail to CloudWatch
cat > cloudtrail-cloudwatch-role-trust.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Service": "cloudtrail.amazonaws.com"
    },
    "Action": "sts:AssumeRole"
  }]
}
EOF

aws iam create-role \
  --role-name CloudTrail-CloudWatch-Role \
  --assume-role-policy-document file://cloudtrail-cloudwatch-role-trust.json

# Attach policy
cat > cloudtrail-cloudwatch-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "logs:CreateLogStream",
      "logs:PutLogEvents"
    ],
    "Resource": "arn:aws:logs:*:*:log-group:/aws/cloudtrail/spawn:*"
  }]
}
EOF

aws iam put-role-policy \
  --role-name CloudTrail-CloudWatch-Role \
  --policy-name CloudWatch-Logs-Policy \
  --policy-document file://cloudtrail-cloudwatch-policy.json

# Update trail with CloudWatch Logs
ROLE_ARN=$(aws iam get-role --role-name CloudTrail-CloudWatch-Role --query 'Role.Arn' --output text)
aws cloudtrail update-trail \
  --name spawn-audit \
  --cloud-watch-logs-log-group-arn arn:aws:logs:us-east-1:ACCOUNT_ID:log-group:/aws/cloudtrail/spawn \
  --cloud-watch-logs-role-arn $ROLE_ARN
```

---

## What Gets Logged

### EC2 Operations

**Instance Lifecycle:**
```json
{
  "eventName": "RunInstances",
  "eventTime": "2026-01-27T20:00:00Z",
  "eventSource": "ec2.amazonaws.com",
  "userIdentity": {
    "type": "IAMUser",
    "principalId": "AIDAI...",
    "arn": "arn:aws:iam::123456789012:user/alice"
  },
  "requestParameters": {
    "instanceType": "c7i.xlarge",
    "instancesSet": {
      "items": [{"imageId": "ami-0abcdef1234567890"}]
    },
    "tagSpecificationSet": {
      "items": [{
        "tags": [
          {"key": "Name", "value": "my-instance"},
          {"key": "spawn:managed", "value": "true"}
        ]
      }]
    }
  },
  "responseElements": {
    "instancesSet": {
      "items": [{
        "instanceId": "i-0abc123def456789"
      }]
    }
  }
}
```

**Instance Termination:**
```json
{
  "eventName": "TerminateInstances",
  "eventTime": "2026-01-27T22:00:00Z",
  "userIdentity": {
    "arn": "arn:aws:iam::123456789012:user/alice"
  },
  "requestParameters": {
    "instancesSet": {
      "items": [{"instanceId": "i-0abc123def456789"}]
    }
  }
}
```

### IAM Operations

**Role Creation:**
```json
{
  "eventName": "CreateRole",
  "eventTime": "2026-01-27T20:00:00Z",
  "userIdentity": {
    "arn": "arn:aws:iam::123456789012:user/alice"
  },
  "requestParameters": {
    "roleName": "spawn-s3-readonly-us-east-1",
    "assumeRolePolicyDocument": "{\"Version\":\"2012-10-17\",\"Statement\":[...]}"
  }
}
```

**Policy Attachment:**
```json
{
  "eventName": "PutRolePolicy",
  "eventTime": "2026-01-27T20:00:01Z",
  "requestParameters": {
    "roleName": "spawn-s3-readonly-us-east-1",
    "policyName": "spawn-s3-readonly-policy"
  }
}
```

### DynamoDB Operations

**State Tracking:**
```json
{
  "eventName": "PutItem",
  "eventTime": "2026-01-27T20:00:02Z",
  "eventSource": "dynamodb.amazonaws.com",
  "requestParameters": {
    "tableName": "spawn-sweeps",
    "item": {
      "sweep_id": {"S": "sweep-20260127-abc123"},
      "user_id": {"S": "alice"},
      "status": {"S": "running"}
    }
  }
}
```

### Lambda Invocations

**Sweep Orchestration:**
```json
{
  "eventName": "Invoke",
  "eventTime": "2026-01-27T20:00:00Z",
  "eventSource": "lambda.amazonaws.com",
  "requestParameters": {
    "functionName": "sweep-orchestrator",
    "invocationType": "Event"
  }
}
```

---

## Querying CloudTrail Logs

### Using AWS CLI

**Find all spawn operations by user:**
```bash
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=Username,AttributeValue=alice \
  --max-items 100 \
  --query 'Events[?contains(EventName, `RunInstances`) || contains(EventName, `TerminateInstances`)]'
```

**Find all instance launches:**
```bash
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=EventName,AttributeValue=RunInstances \
  --start-time 2026-01-27T00:00:00Z \
  --end-time 2026-01-27T23:59:59Z
```

**Find IAM role creations:**
```bash
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=EventName,AttributeValue=CreateRole \
  --query 'Events[?contains(to_string(CloudTrailEvent), `spawn`)]'
```

### Using CloudWatch Logs Insights

**Query for spawn operations:**
```sql
fields @timestamp, userIdentity.arn, eventName, requestParameters.instanceType, responseElements.instancesSet.items.0.instanceId
| filter eventSource = "ec2.amazonaws.com"
| filter eventName in ["RunInstances", "TerminateInstances"]
| filter requestParameters.tagSpecificationSet.items.0.tags.0.key = "spawn:managed"
| sort @timestamp desc
| limit 100
```

**Query for IAM role operations:**
```sql
fields @timestamp, userIdentity.arn, eventName, requestParameters.roleName
| filter eventSource = "iam.amazonaws.com"
| filter eventName in ["CreateRole", "PutRolePolicy", "AttachRolePolicy"]
| filter requestParameters.roleName like /spawn/
| sort @timestamp desc
```

**Query for cost analysis:**
```sql
fields @timestamp, userIdentity.arn, requestParameters.instanceType, count(*) as launch_count
| filter eventName = "RunInstances"
| filter requestParameters.tagSpecificationSet.items.0.tags.0.key = "spawn:managed"
| stats count() by requestParameters.instanceType, userIdentity.arn
```

---

## S3 Log Analysis

CloudTrail logs are stored in S3 in compressed JSON format:

**S3 Path Structure:**
```
s3://my-spawn-audit-logs/
└── AWSLogs/
    └── 123456789012/
        └── CloudTrail/
            └── us-east-1/
                └── 2026/
                    └── 01/
                        └── 27/
                            └── 123456789012_CloudTrail_us-east-1_20260127T2000Z_abc123.json.gz
```

**Download and analyze:**
```bash
# Download logs for a specific day
aws s3 sync s3://my-spawn-audit-logs/AWSLogs/123456789012/CloudTrail/us-east-1/2026/01/27/ ./logs/

# Decompress and search
gunzip -c logs/*.json.gz | jq '.Records[] | select(.eventName == "RunInstances")'

# Count instance launches by user
gunzip -c logs/*.json.gz | jq -r '.Records[] | select(.eventName == "RunInstances") | .userIdentity.arn' | sort | uniq -c
```

---

## Integration with spawn Audit Logs

spawn's built-in audit logging (`pkg/audit`) complements CloudTrail:

**spawn Audit Log (application-level):**
```json
{
  "timestamp": "2026-01-27T20:00:00Z",
  "level": "info",
  "operation": "launch_instances",
  "user_id": "arn:aws:iam::123456789012:user/alice",
  "instance_type": "c7i.xlarge",
  "count": 10,
  "region": "us-east-1",
  "correlation_id": "550e8400-e29b-41d4-a716-446655440000",
  "result": "success"
}
```

**CloudTrail (AWS API-level):**
```json
{
  "eventName": "RunInstances",
  "eventTime": "2026-01-27T20:00:01Z",
  "userIdentity": {
    "arn": "arn:aws:iam::123456789012:user/alice"
  },
  "requestID": "550e8400-e29b-41d4-a716-446655440000",
  "responseElements": {
    "instancesSet": {
      "items": [{"instanceId": "i-0abc123def456789"}]
    }
  }
}
```

**Correlation:** Use correlation IDs or AWS request IDs to link spawn audit logs with CloudTrail events.

---

## CloudWatch Alarms

Set up alarms for security-relevant events:

### Unauthorized Instance Launches

```bash
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-unauthorized-launch \
  --alarm-description "Alert on instance launches without spawn:managed tag" \
  --metric-name RunInstances \
  --namespace AWS/CloudTrail \
  --statistic Sum \
  --period 300 \
  --threshold 1 \
  --comparison-operator GreaterThanThreshold \
  --evaluation-periods 1 \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:security-alerts
```

### IAM Role Modifications

```bash
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-iam-role-modified \
  --alarm-description "Alert on spawn IAM role modifications" \
  --metric-name PutRolePolicy \
  --namespace AWS/CloudTrail \
  --statistic Sum \
  --period 60 \
  --threshold 1 \
  --comparison-operator GreaterThanThreshold
```

---

## Compliance & Retention

### Log Retention

**S3 Lifecycle Policy:**
```json
{
  "Rules": [{
    "Id": "spawn-audit-log-retention",
    "Status": "Enabled",
    "Prefix": "AWSLogs/",
    "Transitions": [
      {
        "Days": 90,
        "StorageClass": "GLACIER"
      }
    ],
    "Expiration": {
      "Days": 2555
    }
  }]
}
```

**Apply policy:**
```bash
aws s3api put-bucket-lifecycle-configuration \
  --bucket my-spawn-audit-logs \
  --lifecycle-configuration file://lifecycle.json
```

### Compliance Requirements

**HIPAA:**
- Retain logs for 6 years (2555 days)
- Enable log file validation
- Encrypt logs at rest (S3-SSE or KMS)

**PCI DSS:**
- Retain logs for 1 year (365 days)
- Implement log review procedures
- Restrict access to audit logs

**SOC 2:**
- Retain logs for duration of audit period + 90 days
- Implement automated log monitoring
- Document log review procedures

---

## Log Security

### S3 Bucket Policy

Restrict access to CloudTrail logs:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "cloudtrail.amazonaws.com"
      },
      "Action": "s3:PutObject",
      "Resource": "arn:aws:s3:::my-spawn-audit-logs/AWSLogs/123456789012/*",
      "Condition": {
        "StringEquals": {
          "s3:x-amz-acl": "bucket-owner-full-control"
        }
      }
    },
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::123456789012:role/SecurityAuditor"
      },
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::my-spawn-audit-logs/AWSLogs/123456789012/*"
    }
  ]
}
```

### Enable S3 Encryption

```bash
aws s3api put-bucket-encryption \
  --bucket my-spawn-audit-logs \
  --server-side-encryption-configuration '{
    "Rules": [{
      "ApplyServerSideEncryptionByDefault": {
        "SSEAlgorithm": "AES256"
      }
    }]
  }'
```

### Enable MFA Delete

```bash
aws s3api put-bucket-versioning \
  --bucket my-spawn-audit-logs \
  --versioning-configuration Status=Enabled,MFADelete=Enabled \
  --mfa "arn:aws:iam::123456789012:mfa/root-account-mfa-device XXXXXX"
```

---

## Troubleshooting

### CloudTrail Not Logging

**Check trail status:**
```bash
aws cloudtrail get-trail-status --name spawn-audit
```

**Common issues:**
- S3 bucket policy missing CloudTrail permissions
- Trail not started (`IsLogging: false`)
- Multi-region trail not configured for global services

**Fix:**
```bash
aws cloudtrail start-logging --name spawn-audit
aws cloudtrail update-trail --name spawn-audit --is-multi-region-trail
```

### Missing Events

**Event selector configuration:**
```bash
# Configure to log all management events
aws cloudtrail put-event-selectors \
  --trail-name spawn-audit \
  --event-selectors '[{
    "ReadWriteType": "All",
    "IncludeManagementEvents": true,
    "DataResources": []
  }]'
```

### High Costs

CloudTrail charges:
- First copy of management events: Free
- Additional copies: $2.00 per 100,000 events
- Data events (S3/Lambda): $0.10 per 100,000 events

**Optimize:**
- Use single trail for all regions (multi-region trail)
- Filter event selectors to spawn-specific operations
- Use S3 lifecycle policies for older logs

---

## Best Practices

1. **Enable multi-region trail** - Capture events from all regions
2. **Enable log file validation** - Detect tampering
3. **Encrypt logs** - Use S3-SSE or KMS encryption
4. **Implement lifecycle policies** - Archive to Glacier, expire old logs
5. **Set up CloudWatch alarms** - Alert on security events
6. **Regular log review** - Audit access patterns and anomalies
7. **Integrate with SIEM** - Ship logs to security monitoring platform
8. **Document procedures** - Incident response, log review, compliance

---

## Related Documentation

- [Security Model](../explanation/security-model.md)
- [SECURITY.md](../../SECURITY.md)
- [Audit Logging (`pkg/audit`)](../../pkg/audit/)
- [AWS CloudTrail Documentation](https://docs.aws.amazon.com/cloudtrail/)
- [CloudWatch Logs Insights](https://docs.aws.amazon.com/AmazonCloudWatch/latest/logs/AnalyzingLogData.html)

---

## Example: Complete Audit Setup

```bash
#!/bin/bash
# Complete CloudTrail audit setup for spawn

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
BUCKET_NAME="spawn-audit-logs-${ACCOUNT_ID}"
TRAIL_NAME="spawn-audit"
REGION="us-east-1"

# 1. Create S3 bucket
aws s3 mb s3://${BUCKET_NAME} --region ${REGION}

# 2. Enable bucket versioning
aws s3api put-bucket-versioning \
  --bucket ${BUCKET_NAME} \
  --versioning-configuration Status=Enabled

# 3. Enable bucket encryption
aws s3api put-bucket-encryption \
  --bucket ${BUCKET_NAME} \
  --server-side-encryption-configuration '{
    "Rules": [{
      "ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}
    }]
  }'

# 4. Apply bucket policy
cat > bucket-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "cloudtrail.amazonaws.com"},
    "Action": "s3:GetBucketAcl",
    "Resource": "arn:aws:s3:::${BUCKET_NAME}"
  }, {
    "Effect": "Allow",
    "Principal": {"Service": "cloudtrail.amazonaws.com"},
    "Action": "s3:PutObject",
    "Resource": "arn:aws:s3:::${BUCKET_NAME}/AWSLogs/${ACCOUNT_ID}/*",
    "Condition": {
      "StringEquals": {"s3:x-amz-acl": "bucket-owner-full-control"}
    }
  }]
}
EOF
aws s3api put-bucket-policy --bucket ${BUCKET_NAME} --policy file://bucket-policy.json

# 5. Create CloudTrail trail
aws cloudtrail create-trail \
  --name ${TRAIL_NAME} \
  --s3-bucket-name ${BUCKET_NAME} \
  --is-multi-region-trail \
  --enable-log-file-validation

# 6. Start logging
aws cloudtrail start-logging --name ${TRAIL_NAME}

# 7. Configure event selectors
aws cloudtrail put-event-selectors \
  --trail-name ${TRAIL_NAME} \
  --event-selectors '[{
    "ReadWriteType": "All",
    "IncludeManagementEvents": true
  }]'

echo "CloudTrail audit logging configured successfully!"
echo "Trail: ${TRAIL_NAME}"
echo "Bucket: ${BUCKET_NAME}"
echo ""
echo "Verify with: aws cloudtrail get-trail-status --name ${TRAIL_NAME}"
```

---

**Last Updated:** 2026-01-27
