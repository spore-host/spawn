# How-To: Disaster Recovery

Backup strategies and disaster recovery procedures for spawn infrastructure.

## Overview

### Disaster Scenarios

1. **Region outage** - AWS region becomes unavailable
2. **Account compromise** - AWS credentials leaked
3. **Data loss** - Accidental deletion of S3 objects or DynamoDB tables
4. **Configuration corruption** - spawn config or infrastructure damaged
5. **Application failure** - Critical bug deployed to production

### Recovery Objectives

- **RTO (Recovery Time Objective):** < 1 hour for critical workloads
- **RPO (Recovery Point Objective):** < 5 minutes for persistent data

---

## Backup Strategy

### What to Back Up

**Infrastructure Account:**
- S3 buckets (spawn-binaries-*, results buckets)
- Lambda function code
- DynamoDB tables (instance metadata, queue state)
- Route53 hosted zone configuration
- IAM roles and policies

**Workload Accounts:**
- Custom AMIs
- DynamoDB tables
- Application data in S3
- Instance configurations (launch templates)

### Backup Schedule

| Component | Frequency | Retention |
|-----------|-----------|-----------|
| S3 data | Continuous (versioning) | 90 days |
| DynamoDB | Point-in-time recovery | 35 days |
| AMIs | Weekly | 30 days |
| Lambda code | On deployment | All versions |
| IAM config | Daily | 90 days |
| Route53 | Weekly | Forever |

---

## S3 Backup

### Enable Versioning

**Critical buckets:**
```bash
#!/bin/bash
# enable-s3-versioning.sh

BUCKETS=(
  "spawn-binaries-us-east-1"
  "spawn-binaries-us-west-2"
  "spawn-results-us-east-1"
)

for BUCKET in "${BUCKETS[@]}"; do
  echo "Enabling versioning on $BUCKET..."

  # Enable versioning
  aws s3api put-bucket-versioning \
    --bucket $BUCKET \
    --versioning-configuration Status=Enabled

  # Enable lifecycle policy to delete old versions
  aws s3api put-bucket-lifecycle-configuration \
    --bucket $BUCKET \
    --lifecycle-configuration '{
      "Rules": [{
        "Id": "DeleteOldVersions",
        "Status": "Enabled",
        "NoncurrentVersionExpiration": {
          "NoncurrentDays": 90
        }
      }]
    }'

  echo "✓ $BUCKET configured"
done
```

### Cross-Region Replication

**Replicate to backup region:**
```bash
#!/bin/bash
# setup-s3-replication.sh

SOURCE_BUCKET="spawn-binaries-us-east-1"
REPLICA_BUCKET="spawn-binaries-us-west-2-backup"
REPLICA_REGION="us-west-2"

# Create replica bucket
aws s3api create-bucket \
  --bucket $REPLICA_BUCKET \
  --region $REPLICA_REGION \
  --create-bucket-configuration LocationConstraint=$REPLICA_REGION

# Enable versioning on replica
aws s3api put-bucket-versioning \
  --bucket $REPLICA_BUCKET \
  --versioning-configuration Status=Enabled

# Create replication role
cat > replication-trust-policy.json << 'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Service": "s3.amazonaws.com"
    },
    "Action": "sts:AssumeRole"
  }]
}
EOF

aws iam create-role \
  --role-name s3-replication-role \
  --assume-role-policy-document file://replication-trust-policy.json

# Attach replication permissions
cat > replication-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetReplicationConfiguration",
        "s3:ListBucket"
      ],
      "Resource": "arn:aws:s3:::$SOURCE_BUCKET"
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObjectVersionForReplication",
        "s3:GetObjectVersionAcl"
      ],
      "Resource": "arn:aws:s3:::$SOURCE_BUCKET/*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:ReplicateObject",
        "s3:ReplicateDelete"
      ],
      "Resource": "arn:aws:s3:::$REPLICA_BUCKET/*"
    }
  ]
}
EOF

aws iam put-role-policy \
  --role-name s3-replication-role \
  --policy-name replication-policy \
  --policy-document file://replication-policy.json

# Get role ARN
ROLE_ARN=$(aws iam get-role --role-name s3-replication-role --query 'Role.Arn' --output text)

# Configure replication
cat > replication-config.json << EOF
{
  "Role": "$ROLE_ARN",
  "Rules": [{
    "Status": "Enabled",
    "Priority": 1,
    "DeleteMarkerReplication": {
      "Status": "Enabled"
    },
    "Filter": {},
    "Destination": {
      "Bucket": "arn:aws:s3:::$REPLICA_BUCKET",
      "ReplicationTime": {
        "Status": "Enabled",
        "Time": {
          "Minutes": 15
        }
      },
      "Metrics": {
        "Status": "Enabled",
        "EventThreshold": {
          "Minutes": 15
        }
      }
    }
  }]
}
EOF

aws s3api put-bucket-replication \
  --bucket $SOURCE_BUCKET \
  --replication-configuration file://replication-config.json

echo "Replication configured: $SOURCE_BUCKET → $REPLICA_BUCKET"
```

### Backup to Glacier

**Long-term archival:**
```bash
# Lifecycle policy to move old data to Glacier
aws s3api put-bucket-lifecycle-configuration \
  --bucket spawn-results-us-east-1 \
  --lifecycle-configuration '{
    "Rules": [{
      "Id": "ArchiveOldResults",
      "Status": "Enabled",
      "Filter": {
        "Prefix": "results/"
      },
      "Transitions": [{
        "Days": 30,
        "StorageClass": "GLACIER"
      }, {
        "Days": 90,
        "StorageClass": "DEEP_ARCHIVE"
      }]
    }]
  }'
```

---

## DynamoDB Backup

### Point-in-Time Recovery

**Enable PITR:**
```bash
#!/bin/bash
# enable-dynamodb-pitr.sh

TABLES=(
  "spawn-instances"
  "spawn-queues"
  "spawn-alerts"
)

for TABLE in "${TABLES[@]}"; do
  echo "Enabling PITR for $TABLE..."

  aws dynamodb update-continuous-backups \
    --table-name $TABLE \
    --point-in-time-recovery-specification PointInTimeRecoveryEnabled=true

  echo "✓ $TABLE configured"
done
```

**Restore from PITR:**
```bash
# Restore table to specific point in time
RESTORE_TIME="2026-01-27T10:30:00Z"

aws dynamodb restore-table-to-point-in-time \
  --source-table-name spawn-instances \
  --target-table-name spawn-instances-restored \
  --restore-date-time $RESTORE_TIME

echo "Restoring table to $RESTORE_TIME..."

# Wait for restore to complete
aws dynamodb wait table-exists --table-name spawn-instances-restored

echo "Restore complete"
```

### On-Demand Backups

**Create backup before major changes:**
```bash
#!/bin/bash
# backup-dynamodb.sh

TABLES=(
  "spawn-instances"
  "spawn-queues"
  "spawn-alerts"
)

BACKUP_SUFFIX=$(date +%Y%m%d-%H%M%S)

for TABLE in "${TABLES[@]}"; do
  echo "Backing up $TABLE..."

  aws dynamodb create-backup \
    --table-name $TABLE \
    --backup-name "${TABLE}-backup-${BACKUP_SUFFIX}"

  echo "✓ $TABLE backed up"
done
```

**Restore from backup:**
```bash
# List available backups
aws dynamodb list-backups --table-name spawn-instances

# Restore from backup
BACKUP_ARN="arn:aws:dynamodb:us-east-1:123456789012:table/spawn-instances/backup/01234567890123-abcdefgh"

aws dynamodb restore-table-from-backup \
  --target-table-name spawn-instances-restored \
  --backup-arn $BACKUP_ARN
```

### Cross-Region Backup

**Use global tables:**
```bash
# Create global table (replicates to us-west-2)
aws dynamodb update-table \
  --table-name spawn-instances \
  --replica-updates '[{
    "Create": {
      "RegionName": "us-west-2"
    }
  }]'

# Verify replication
aws dynamodb describe-table --table-name spawn-instances \
  --query 'Table.Replicas'
```

---

## AMI Backup

### Automated AMI Snapshots

**Create backup AMIs:**
```bash
#!/bin/bash
# backup-amis.sh

# Find all custom AMIs
AMIS=$(aws ec2 describe-images \
  --owners self \
  --filters "Name=tag:spawn,Values=true" \
  --query 'Images[*].[ImageId,Name]' \
  --output text)

while IFS=$'\t' read -r AMI_ID AMI_NAME; do
  echo "Creating snapshot of $AMI_NAME ($AMI_ID)..."

  # Copy AMI to backup region
  NEW_AMI_ID=$(aws ec2 copy-image \
    --source-region us-east-1 \
    --source-image-id $AMI_ID \
    --region us-west-2 \
    --name "${AMI_NAME}-backup-$(date +%Y%m%d)" \
    --query 'ImageId' \
    --output text)

  # Tag backup AMI
  aws ec2 create-tags \
    --region us-west-2 \
    --resources $NEW_AMI_ID \
    --tags Key=backup,Value=true Key=source-ami,Value=$AMI_ID

  echo "✓ Backup AMI created: $NEW_AMI_ID"
done <<< "$AMIS"
```

**Automated via Lambda:**
```python
# lambda/ami-backup/lambda_function.py
import boto3
import os
from datetime import datetime

ec2 = boto3.client('ec2')
BACKUP_REGION = os.environ['BACKUP_REGION']

def lambda_handler(event, context):
    """Backup all spawn AMIs to secondary region"""

    # Find custom AMIs
    response = ec2.describe_images(
        Owners=['self'],
        Filters=[{'Name': 'tag:spawn', 'Values': ['true']}]
    )

    ec2_backup = boto3.client('ec2', region_name=BACKUP_REGION)

    for image in response['Images']:
        ami_id = image['ImageId']
        ami_name = image['Name']

        print(f"Backing up {ami_name} ({ami_id})")

        # Copy to backup region
        backup_name = f"{ami_name}-backup-{datetime.now().strftime('%Y%m%d')}"

        new_ami = ec2_backup.copy_image(
            SourceRegion='us-east-1',
            SourceImageId=ami_id,
            Name=backup_name,
            Description=f"Backup of {ami_id}"
        )

        # Tag backup
        ec2_backup.create_tags(
            Resources=[new_ami['ImageId']],
            Tags=[
                {'Key': 'backup', 'Value': 'true'},
                {'Key': 'source-ami', 'Value': ami_id}
            ]
        )

        print(f"Created backup: {new_ami['ImageId']}")

    return {'statusCode': 200, 'body': 'AMI backup complete'}
```

**Schedule via EventBridge:**
```bash
# Weekly AMI backup
aws events put-rule \
  --name spawn-weekly-ami-backup \
  --schedule-expression "cron(0 2 ? * SUN *)" \
  --state ENABLED

aws lambda add-permission \
  --function-name ami-backup \
  --statement-id AllowEventBridge \
  --action lambda:InvokeFunction \
  --principal events.amazonaws.com \
  --source-arn arn:aws:events:us-east-1:123456789012:rule/spawn-weekly-ami-backup

aws events put-targets \
  --rule spawn-weekly-ami-backup \
  --targets "Id"="1","Arn"="arn:aws:lambda:us-east-1:123456789012:function:ami-backup"
```

---

## Lambda Function Backup

### Version Control

**Deploy with versions:**
```bash
#!/bin/bash
# deploy-lambda.sh

FUNCTION_NAME="spawn-dns-updater"

# Upload new code
aws lambda update-function-code \
  --function-name $FUNCTION_NAME \
  --zip-file fileb://function.zip

# Wait for update to complete
aws lambda wait function-updated --function-name $FUNCTION_NAME

# Publish new version
VERSION=$(aws lambda publish-version \
  --function-name $FUNCTION_NAME \
  --description "Deployed $(date)" \
  --query 'Version' \
  --output text)

echo "Published version: $VERSION"

# Tag production version
aws lambda update-alias \
  --function-name $FUNCTION_NAME \
  --name production \
  --function-version $VERSION

echo "Production alias updated to version $VERSION"
```

**Rollback to previous version:**
```bash
# List versions
aws lambda list-versions-by-function --function-name spawn-dns-updater

# Rollback production alias to previous version
aws lambda update-alias \
  --function-name spawn-dns-updater \
  --name production \
  --function-version 5  # Previous stable version
```

### Export Lambda Configuration

**Backup Lambda config:**
```bash
#!/bin/bash
# export-lambda-config.sh

FUNCTIONS=$(aws lambda list-functions --query 'Functions[*].FunctionName' --output text)

for FUNCTION in $FUNCTIONS; do
  echo "Exporting $FUNCTION..."

  # Get function configuration
  aws lambda get-function --function-name $FUNCTION > "lambda-${FUNCTION}.json"

  # Download function code
  CODE_URL=$(jq -r '.Code.Location' "lambda-${FUNCTION}.json")
  curl -L "$CODE_URL" -o "lambda-${FUNCTION}.zip"

  echo "✓ $FUNCTION exported"
done

# Upload to S3
aws s3 sync . s3://spawn-backups/lambda/ --exclude "*" --include "lambda-*.json" --include "lambda-*.zip"

echo "Lambda backups uploaded to S3"
```

---

## IAM Configuration Backup

### Export IAM Policies

**Backup IAM roles:**
```bash
#!/bin/bash
# backup-iam.sh

BACKUP_DIR="iam-backup-$(date +%Y%m%d)"
mkdir -p $BACKUP_DIR

# Export all IAM roles
ROLES=$(aws iam list-roles --query 'Roles[?starts_with(RoleName, `spawn`)].RoleName' --output text)

for ROLE in $ROLES; do
  echo "Exporting role: $ROLE"

  # Get role details
  aws iam get-role --role-name $ROLE > "$BACKUP_DIR/role-${ROLE}.json"

  # Get attached policies
  aws iam list-attached-role-policies --role-name $ROLE > "$BACKUP_DIR/role-${ROLE}-attached-policies.json"

  # Get inline policies
  POLICIES=$(aws iam list-role-policies --role-name $ROLE --query 'PolicyNames' --output text)

  for POLICY in $POLICIES; do
    aws iam get-role-policy --role-name $ROLE --policy-name $POLICY > "$BACKUP_DIR/role-${ROLE}-policy-${POLICY}.json"
  done
done

# Create tarball
tar czf iam-backup-$(date +%Y%m%d).tar.gz $BACKUP_DIR/

# Upload to S3
aws s3 cp iam-backup-$(date +%Y%m%d).tar.gz s3://spawn-backups/iam/

echo "IAM backup complete"
```

---

## Route53 Backup

### Export DNS Configuration

**Backup hosted zone:**
```bash
#!/bin/bash
# backup-route53.sh

HOSTED_ZONE_ID="/hostedzone/Z1234567890ABC"

# Export all records
aws route53 list-resource-record-sets \
  --hosted-zone-id $HOSTED_ZONE_ID \
  > route53-backup-$(date +%Y%m%d).json

# Upload to S3
aws s3 cp route53-backup-$(date +%Y%m%d).json s3://spawn-backups/route53/

echo "Route53 backup complete"
```

**Restore DNS records:**
```bash
#!/bin/bash
# restore-route53.sh

BACKUP_FILE=$1
HOSTED_ZONE_ID=$2

# Convert to change batch format
jq '{
  "Changes": [
    .ResourceRecordSets[] |
    select(.Type != "NS" and .Type != "SOA") |
    {
      "Action": "UPSERT",
      "ResourceRecordSet": .
    }
  ]
}' $BACKUP_FILE > changes.json

# Apply changes
aws route53 change-resource-record-sets \
  --hosted-zone-id $HOSTED_ZONE_ID \
  --change-batch file://changes.json

echo "DNS records restored"
```

---

## Disaster Recovery Procedures

### Scenario 1: Region Outage

**Impact:** us-east-1 unavailable, all instances lost.

**Recovery Steps:**

1. **Switch to backup region:**
```bash
# Launch instances in us-west-2
export AWS_REGION=us-west-2

spawn launch \
  --instance-type c7i.xlarge \
  --ami ami-backup-xxx \
  --ttl 2h
```

2. **Update DNS to point to new region:**
```bash
# Update Route53 to use us-west-2 instances
# (Automatic if using health checks and failover routing)
```

3. **Resume workloads from checkpoints:**
```bash
# Download checkpoint from S3 (replicated to us-west-2)
aws s3 cp s3://spawn-results-us-west-2/checkpoint.pkl /tmp/

# Resume processing
python resume.py --checkpoint /tmp/checkpoint.pkl
```

**Recovery Time:** ~15 minutes

---

### Scenario 2: Accidental Data Deletion

**Impact:** S3 bucket accidentally emptied.

**Recovery Steps:**

1. **Restore from S3 versions:**
```bash
# List deleted objects
aws s3api list-object-versions \
  --bucket spawn-binaries-us-east-1 \
  --query 'DeleteMarkers[*].[Key,VersionId]'

# Restore objects
aws s3api delete-object \
  --bucket spawn-binaries-us-east-1 \
  --key spored \
  --version-id <delete-marker-version-id>
```

2. **Or restore from replica bucket:**
```bash
# Sync from replica
aws s3 sync s3://spawn-binaries-us-west-2-backup/ s3://spawn-binaries-us-east-1/
```

**Recovery Time:** ~5 minutes

---

### Scenario 3: Account Compromise

**Impact:** AWS credentials leaked, attacker has access.

**Recovery Steps:**

1. **Immediately rotate credentials:**
```bash
# Deactivate compromised keys
aws iam update-access-key \
  --access-key-id AKIAIOSFODNN7EXAMPLE \
  --status Inactive

# Generate new keys
aws iam create-access-key --user-name spawn-user
```

2. **Audit CloudTrail for malicious activity:**
```bash
# Check for unauthorized actions
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=Username,AttributeValue=spawn-user \
  --start-time $(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%S) \
  --max-results 50
```

3. **Terminate suspicious instances:**
```bash
# List recent launches
spawn list --filter "launch_time > $(date -d '1 hour ago' +%s)"

# Terminate suspicious instances
spawn cancel <instance-id>
```

4. **Restore IAM policies from backup:**
```bash
# Download backup
aws s3 cp s3://spawn-backups/iam/iam-backup-latest.tar.gz .
tar xzf iam-backup-latest.tar.gz

# Restore policies
# (Manual review recommended before applying)
```

**Recovery Time:** ~30 minutes

---

### Scenario 4: DynamoDB Table Corruption

**Impact:** spawn-instances table contains corrupted data.

**Recovery Steps:**

1. **Restore from PITR:**
```bash
# Identify corruption time
# Restore to 5 minutes before corruption

aws dynamodb restore-table-to-point-in-time \
  --source-table-name spawn-instances \
  --target-table-name spawn-instances-restored \
  --restore-date-time 2026-01-27T10:25:00Z

# Wait for restore
aws dynamodb wait table-exists --table-name spawn-instances-restored
```

2. **Swap tables:**
```bash
# Rename current table
aws dynamodb update-table \
  --table-name spawn-instances \
  --table-name spawn-instances-corrupted

# Rename restored table
aws dynamodb update-table \
  --table-name spawn-instances-restored \
  --table-name spawn-instances
```

**Recovery Time:** ~10 minutes

---

## Automated DR Testing

### Monthly DR Drill

**Simulate region failure:**
```bash
#!/bin/bash
# dr-drill.sh

echo "=== Disaster Recovery Drill ==="
echo "Simulating us-east-1 outage..."

# 1. Launch workload in backup region
echo "Launching instances in us-west-2..."
export AWS_REGION=us-west-2

INSTANCE_ID=$(spawn launch \
  --instance-type c7i.xlarge \
  --ami ami-backup-xxx \
  --wait-for-ssh \
  --quiet)

echo "Instance launched: $INSTANCE_ID"

# 2. Verify can access replicated S3 data
echo "Verifying S3 replication..."
spawn connect $INSTANCE_ID -c "
  aws s3 ls s3://spawn-binaries-us-west-2-backup/
  aws s3 cp s3://spawn-binaries-us-west-2-backup/spored /tmp/spored
  [ -f /tmp/spored ] && echo 'S3 replication: OK' || echo 'S3 replication: FAILED'
"

# 3. Verify DynamoDB global table accessible
echo "Verifying DynamoDB replication..."
aws dynamodb get-item \
  --region us-west-2 \
  --table-name spawn-instances \
  --key '{"instance_id":{"S":"'$INSTANCE_ID'"}}' \
  && echo "DynamoDB replication: OK" || echo "DynamoDB replication: FAILED"

# 4. Cleanup
echo "Cleaning up..."
aws ec2 terminate-instances --region us-west-2 --instance-ids $INSTANCE_ID

echo "=== DR Drill Complete ==="
```

**Schedule monthly:**
```bash
# Add to crontab
0 3 1 * * /usr/local/bin/dr-drill.sh > /var/log/dr-drill.log 2>&1
```

---

## Backup Monitoring

### CloudWatch Alarms

**Alert on replication lag:**
```bash
# S3 replication time alarm
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-s3-replication-lag \
  --alarm-description "S3 replication taking > 15 minutes" \
  --metric-name ReplicationLatency \
  --namespace AWS/S3 \
  --statistic Maximum \
  --period 900 \
  --evaluation-periods 1 \
  --threshold 900000 \
  --comparison-operator GreaterThanThreshold \
  --dimensions Name=SourceBucket,Value=spawn-binaries-us-east-1 \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-alerts
```

**Alert on backup failures:**
```bash
# Lambda backup function errors
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-backup-failures \
  --alarm-description "Backup Lambda function failing" \
  --metric-name Errors \
  --namespace AWS/Lambda \
  --statistic Sum \
  --period 300 \
  --evaluation-periods 1 \
  --threshold 1 \
  --comparison-operator GreaterThanThreshold \
  --dimensions Name=FunctionName,Value=ami-backup \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-critical
```

---

## Best Practices

### 1. Automate Everything
```bash
# Don't rely on manual backups
# Use EventBridge schedules for automated backups
```

### 2. Test Restores Regularly
```bash
# Monthly DR drills
# Verify backups are restorable
```

### 3. Multi-Region by Default
```bash
# Always replicate critical data
# Use global tables for DynamoDB
# Use S3 cross-region replication
```

### 4. Immutable Infrastructure
```bash
# Store everything in version control
# Rebuild from code, not from backups
```

### 5. Monitor Backup Health
```bash
# CloudWatch alarms for replication lag
# SNS notifications on backup failures
```

---

## See Also

- [How-To: Multi-Account Setup](multi-account.md) - Account isolation
- [How-To: Security & IAM](security-iam.md) - Security practices
- [How-To: Custom AMIs](custom-amis.md) - AMI management
- [AWS Disaster Recovery Whitepaper](https://docs.aws.amazon.com/whitepapers/latest/disaster-recovery-workloads-on-aws/)
