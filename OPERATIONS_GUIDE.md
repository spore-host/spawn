# spawn Operations Guide

Comprehensive guide for operating spawn in production environments, covering monitoring, cost management, security, performance, and incident response.

## Table of Contents

1. [Monitoring](#monitoring)
2. [Cost Management](#cost-management)
3. [Security Operations](#security-operations)
4. [Performance Tuning](#performance-tuning)
5. [Backup & Recovery](#backup--recovery)
6. [Scaling Strategies](#scaling-strategies)
7. [Maintenance](#maintenance)
8. [Incident Response](#incident-response)

---

## Monitoring

### CloudWatch Metrics

spawn instances automatically report custom CloudWatch metrics for monitoring and alerting.

#### Key Metrics

**Instance-Level Metrics:**
```bash
# CPU utilization (%)
spawn/cpu_utilization

# Memory utilization (%)
spawn/memory_utilization

# Disk I/O (read/write MB/s)
spawn/disk_read_mb
spawn/disk_write_mb

# Network I/O (MB/s)
spawn/network_in_mb
spawn/network_out_mb

# GPU utilization (%) - GPU instances only
spawn/gpu_utilization
```

**Sweep-Level Metrics:**
```bash
# Active instances in sweep
spawn/sweep/active_instances

# Completed instances in sweep
spawn/sweep/completed_instances

# Failed instances in sweep
spawn/sweep/failed_instances

# Total sweep cost (USD)
spawn/sweep/cost_usd
```

#### View Metrics

**Via AWS CLI:**
```bash
# Get CPU utilization for instance
aws cloudwatch get-metric-statistics \
  --namespace spawn \
  --metric-name cpu_utilization \
  --dimensions Name=InstanceId,Value=i-1234567890abcdef0 \
  --start-time 2026-01-22T00:00:00Z \
  --end-time 2026-01-22T23:59:59Z \
  --period 300 \
  --statistics Average

# Get sweep-level metrics
aws cloudwatch get-metric-statistics \
  --namespace spawn \
  --metric-name active_instances \
  --dimensions Name=SweepID,Value=hyperparam-20260122-abc123 \
  --start-time 2026-01-22T00:00:00Z \
  --end-time 2026-01-22T23:59:59Z \
  --period 60 \
  --statistics Sum
```

**Via CloudWatch Console:**
1. Open CloudWatch console
2. Navigate to "Metrics" → "All metrics"
3. Select "spawn" namespace
4. Choose metric dimension (InstanceId, SweepID, etc.)

### Log Aggregation

#### CloudWatch Logs

All spawn instances send logs to CloudWatch Logs for centralized monitoring.

**Log Groups:**
```
/spawn/instances/{instance-id}
/spawn/sweeps/{sweep-id}
/spawn/dns-updater
```

**Query Logs:**
```bash
# Get logs for specific instance
aws logs filter-log-events \
  --log-group-name /spawn/instances/i-1234567890abcdef0 \
  --start-time $(date -u -d '1 hour ago' +%s)000 \
  --filter-pattern "ERROR"

# Get sweep summary
aws logs filter-log-events \
  --log-group-name /spawn/sweeps/hyperparam-20260122-abc123 \
  --filter-pattern "[timestamp, level=ERROR, ...]"
```

**Insights Queries:**
```sql
-- Find all errors in last hour
fields @timestamp, @message
| filter @message like /ERROR/
| sort @timestamp desc
| limit 100

-- Sweep completion summary
fields @timestamp, instance_id, status, duration
| filter sweep_id = "hyperparam-20260122-abc123"
| stats count() by status

-- Cost analysis
fields @timestamp, instance_type, region, cost_usd
| stats sum(cost_usd) by instance_type, region
```

### Alert Setup

#### CloudWatch Alarms

**High CPU Usage:**
```bash
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-high-cpu \
  --alarm-description "Alert when instance CPU > 90%" \
  --metric-name cpu_utilization \
  --namespace spawn \
  --statistic Average \
  --period 300 \
  --evaluation-periods 2 \
  --threshold 90 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-alerts
```

**Sweep Failure Rate:**
```bash
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-sweep-failures \
  --alarm-description "Alert when > 10% of sweep instances fail" \
  --metrics file://sweep-failure-metric.json \
  --evaluation-periods 1 \
  --threshold 10 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-alerts
```

**Cost Overrun:**
```bash
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-cost-overrun \
  --alarm-description "Alert when sweep cost > $1000" \
  --metric-name cost_usd \
  --namespace spawn \
  --dimensions Name=SweepID,Value=* \
  --statistic Sum \
  --period 3600 \
  --evaluation-periods 1 \
  --threshold 1000 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-cost-alerts
```

### Dashboards

#### CloudWatch Dashboard JSON

Create comprehensive monitoring dashboard:

```bash
aws cloudwatch put-dashboard --dashboard-name spawn-overview --dashboard-body file://dashboard.json
```

**dashboard.json:**
```json
{
  "widgets": [
    {
      "type": "metric",
      "properties": {
        "metrics": [
          ["spawn", "active_instances", {"stat": "Sum"}]
        ],
        "period": 60,
        "stat": "Sum",
        "region": "us-east-1",
        "title": "Active Instances"
      }
    },
    {
      "type": "metric",
      "properties": {
        "metrics": [
          ["spawn", "cost_usd", {"stat": "Sum"}]
        ],
        "period": 3600,
        "stat": "Sum",
        "region": "us-east-1",
        "title": "Hourly Cost"
      }
    }
  ]
}
```

---

## Cost Management

### Budget Tracking

#### AWS Budgets

**Set Up Budget:**
```bash
aws budgets create-budget \
  --account-id 123456789012 \
  --budget file://spawn-budget.json \
  --notifications-with-subscribers file://notifications.json
```

**spawn-budget.json:**
```json
{
  "BudgetName": "spawn-monthly",
  "BudgetLimit": {
    "Amount": "10000",
    "Unit": "USD"
  },
  "TimeUnit": "MONTHLY",
  "BudgetType": "COST",
  "CostFilters": {
    "TagKey": ["spawn:managed"]
  }
}
```

### Cost Allocation Tags

**Required Tags:**
```yaml
spawn:managed: "true"
spawn:sweep-id: "hyperparam-20260122-abc123"
spawn:sweep-name: "hyperparam"
spawn:owner: "username"
spawn:project: "ml-training"
```

**Enable Cost Allocation Tags:**
```bash
aws ce update-cost-allocation-tags-status \
  --cost-allocation-tags-status TagKey=spawn:sweep-id,Status=Active \
  --cost-allocation-tags-status TagKey=spawn:project,Status=Active
```

### Cost Reports

#### Daily Cost Report

**Generate Report:**
```bash
aws ce get-cost-and-usage \
  --time-period Start=2026-01-22,End=2026-01-23 \
  --granularity DAILY \
  --metrics "UnblendedCost" \
  --group-by Type=TAG,Key=spawn:sweep-id \
  --filter file://spawn-filter.json
```

**spawn-filter.json:**
```json
{
  "Tags": {
    "Key": "spawn:managed",
    "Values": ["true"]
  }
}
```

#### Cost by Instance Type

```bash
aws ce get-cost-and-usage \
  --time-period Start=2026-01-01,End=2026-01-31 \
  --granularity MONTHLY \
  --metrics "UnblendedCost" "UsageQuantity" \
  --group-by Type=DIMENSION,Key=INSTANCE_TYPE \
  --filter file://spawn-filter.json \
  --output table
```

### Cost Optimization

**Strategies:**

1. **Use Spot Instances:**
   - 70-90% cost savings
   - Best for fault-tolerant workloads
   ```bash
   spawn launch --params sweep.yaml --spot
   ```

2. **Right-Size Instances:**
   - Monitor utilization metrics
   - Downsize under-utilized instances
   ```bash
   # Check CPU/memory utilization
   aws cloudwatch get-metric-statistics \
     --namespace spawn \
     --metric-name cpu_utilization \
     --dimensions Name=InstanceType,Value=c5.4xlarge
   ```

3. **Auto-Termination:**
   - Use TTL for time-bound workloads
   - Enable idle detection
   ```yaml
   ttl: 8h
   idle_timeout: 30m
   hibernate_on_idle: true
   ```

4. **Data Staging:**
   - 90-99% data transfer savings
   - Break-even at 1 instance per region
   ```bash
   spawn stage upload dataset.tar.gz --regions us-east-1,us-west-2
   ```

---

## Security Operations

### IAM Role Management

#### Instance Profile

**Minimal Permissions:**
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject"
      ],
      "Resource": "arn:aws:s3:::spawn-results-*/*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem"
      ],
      "Resource": "arn:aws:dynamodb:*:*:table/spawn-sweeps"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeTags"
      ],
      "Resource": "*"
    }
  ]
}
```

**Create Role:**
```bash
aws iam create-role \
  --role-name spawn-instance-role \
  --assume-role-policy-document file://trust-policy.json

aws iam put-role-policy \
  --role-name spawn-instance-role \
  --policy-name spawn-permissions \
  --policy-document file://permissions.json

aws iam create-instance-profile \
  --instance-profile-name spawn-instance-profile

aws iam add-role-to-instance-profile \
  --instance-profile-name spawn-instance-profile \
  --role-name spawn-instance-role
```

### Key Rotation

**SSH Key Rotation:**
```bash
# Generate new key
aws ec2 create-key-pair --key-name spawn-key-2026-02 --query 'KeyMaterial' --output text > spawn-key-2026-02.pem
chmod 400 spawn-key-2026-02.pem

# Update default key
export SPAWN_KEY_NAME=spawn-key-2026-02

# Decommission old key (after migration)
aws ec2 delete-key-pair --key-name spawn-key-2026-01
```

### Access Auditing

**CloudTrail Logs:**
```bash
# Find who launched instances
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=EventName,AttributeValue=RunInstances \
  --start-time 2026-01-22T00:00:00Z \
  --end-time 2026-01-22T23:59:59Z \
  --query 'Events[?contains(CloudTrailEvent, `spawn`)].{Time:EventTime,User:Username,Event:EventName}'
```

**Access Logs:**
```bash
# Review S3 access logs
aws s3api get-bucket-logging --bucket spawn-results-123456789012-us-east-1

# Review EC2 instance metadata access
aws logs filter-log-events \
  --log-group-name /spawn/instances/* \
  --filter-pattern "169.254.169.254"
```

### Compliance Checks

**Security Scan:**
```bash
# Check for public instances
aws ec2 describe-instances \
  --filters "Name=tag:spawn:managed,Values=true" \
  --query 'Reservations[].Instances[?PublicIpAddress!=`null`].InstanceId'

# Check IAM role attachment
aws ec2 describe-instances \
  --filters "Name=tag:spawn:managed,Values=true" \
  --query 'Reservations[].Instances[?IamInstanceProfile==`null`].InstanceId'

# Check for outdated AMIs
aws ec2 describe-instances \
  --filters "Name=tag:spawn:managed,Values=true" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].[InstanceId,ImageId,LaunchTime]' \
  --output table
```

---

## Performance Tuning

### Instance Type Selection

**Decision Matrix:**

| Workload | Bottleneck | Instance Family | Example |
|----------|------------|-----------------|---------|
| CPU-bound | Compute | C5, C6i, C7g | c5.4xlarge |
| Memory-bound | RAM | R5, R6i, X2 | r5.2xlarge |
| GPU training | GPU compute | P3, P4d, P5 | p3.2xlarge |
| GPU inference | GPU memory | G4dn, G5 | g4dn.xlarge |
| MPI/HPC | Network | C5n, C6gn | c5n.18xlarge |
| Storage I/O | Disk | I3, I4i, Im4gn | i3.4xlarge |

**Benchmarking:**
```bash
# CPU benchmark
spawn launch --instance-type c5.2xlarge --command "sysbench cpu run"
spawn launch --instance-type c5.4xlarge --command "sysbench cpu run"

# Compare results
spawn collect --metric benchmark_score --output benchmark.csv
```

### Region Selection

**Latency Testing:**
```bash
# Test latency to S3 buckets
for region in us-east-1 us-west-2 eu-west-1; do
  time aws s3 ls s3://my-data-$region/ --region $region
done
```

**Cost by Region:**
| Region | On-Demand c5.4xlarge | Spot c5.4xlarge | Data Transfer (per GB) |
|--------|---------------------|-----------------|----------------------|
| us-east-1 | $0.68/hr | ~$0.20/hr | $0.09 |
| us-west-2 | $0.68/hr | ~$0.22/hr | $0.09 |
| eu-west-1 | $0.76/hr | ~$0.24/hr | $0.09 |

### Launch Parallelism

**Optimize Launch Speed:**

spawn launches instances in parallel with automatic throttling:

```yaml
# Default: 50 instances per region in parallel
count: 200
regions:
  - us-east-1: 100
  - us-west-2: 100

# Launches in ~60 seconds (vs 400+ seconds serial)
```

**Quotas:**
- Check vCPU limits: `aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A`
- Request increase if needed: `aws service-quotas request-service-quota-increase`

---

## Backup & Recovery

### AMI Management

**Snapshot Strategy:**
```bash
# Create AMI from configured instance
aws ec2 create-image \
  --instance-id i-1234567890abcdef0 \
  --name "spawn-ml-training-$(date +%Y%m%d)" \
  --description "ML training environment with PyTorch 2.0"

# Tag AMI
aws ec2 create-tags \
  --resources ami-0abcdef1234567890 \
  --tags Key=spawn:ami-type,Value=ml-training Key=spawn:version,Value=2.0
```

**AMI Lifecycle:**
```bash
# List old AMIs
aws ec2 describe-images \
  --owners self \
  --filters "Name=tag:spawn:ami-type,Values=*" \
  --query 'Images[?CreationDate<=`2025-12-01`].[ImageId,Name,CreationDate]'

# Deregister old AMIs
aws ec2 deregister-image --image-id ami-old123456
```

### Data Backup

**S3 Versioning:**
```bash
# Enable versioning on results bucket
aws s3api put-bucket-versioning \
  --bucket spawn-results-123456789012-us-east-1 \
  --versioning-configuration Status=Enabled

# List versions
aws s3api list-object-versions \
  --bucket spawn-results-123456789012-us-east-1 \
  --prefix sweeps/hyperparam-20260122-abc123/
```

**Cross-Region Replication:**
```bash
# Replicate critical results
aws s3api put-bucket-replication \
  --bucket spawn-results-123456789012-us-east-1 \
  --replication-configuration file://replication.json
```

### State Restoration

**DynamoDB Backups:**
```bash
# Enable point-in-time recovery
aws dynamodb update-continuous-backups \
  --table-name spawn-sweeps \
  --point-in-time-recovery-specification PointInTimeRecoveryEnabled=true

# Create on-demand backup
aws dynamodb create-backup \
  --table-name spawn-sweeps \
  --backup-name spawn-sweeps-$(date +%Y%m%d)

# Restore from backup
aws dynamodb restore-table-from-backup \
  --target-table-name spawn-sweeps-restored \
  --backup-arn arn:aws:dynamodb:us-east-1:123456789012:table/spawn-sweeps/backup/01640000000000-abcdefgh
```

---

## Scaling Strategies

### Large Job Arrays

**10k+ Instances:**

Strategies for massive-scale deployments:

1. **Multi-Region Distribution:**
```yaml
count: 10000
regions:
  - us-east-1: 3000
  - us-west-2: 3000
  - eu-west-1: 2000
  - ap-southeast-1: 2000
```

2. **Quota Management:**
```bash
# Check current limits
aws service-quotas list-service-quotas --service-code ec2 \
  | grep -A 5 "L-1216C47A"  # Running On-Demand instances

# Request increase to 5000 vCPUs per region
aws service-quotas request-service-quota-increase \
  --service-code ec2 \
  --quota-code L-1216C47A \
  --desired-value 5000 \
  --region us-east-1
```

3. **Launch in Batches:**
```bash
# Launch 10k instances in 20 batches of 500
for i in {1..20}; do
  spawn launch --params sweep.yaml --count 500 --batch-id $i
  sleep 60  # Throttle to avoid API limits
done
```

### Multi-Account Deployments

**AWS Organizations:**

Use separate accounts for different teams/projects:

```
Production Account (123456789012)
└── spawn-prod-team-a
└── spawn-prod-team-b

Development Account (234567890123)
└── spawn-dev-team-a
└── spawn-dev-team-b
```

**Cross-Account Access:**
```bash
# Configure profile for each account
aws configure --profile spawn-prod
aws configure --profile spawn-dev

# Launch in specific account
AWS_PROFILE=spawn-prod spawn launch --params sweep.yaml
```

### Regional Distribution

**Strategy by Use Case:**

**Latency-Sensitive (< 10ms):**
- Single region
- Use placement groups
- Enable EFA for MPI

**Cost-Optimized:**
- Multi-region for spot diversity
- Prefer cheapest regions
- Use data staging

**Global Distribution:**
- Multi-region for redundancy
- Local data processing
- Cross-region results aggregation

---

## Maintenance

### Upgrades

**spawn CLI:**
```bash
# Check current version
spawn --version

# Upgrade via Homebrew
brew upgrade spawn

# Upgrade via pip
pip install --upgrade spawn-cli

# Verify upgrade
spawn --version
```

**AMI Updates:**
```bash
# Find latest Amazon Linux 2023 AMI
aws ec2 describe-images \
  --owners amazon \
  --filters "Name=name,Values=al2023-ami-*-x86_64" \
  --query 'Images | sort_by(@, &CreationDate) | [-1].[ImageId,Name,CreationDate]'

# Update default AMI in config
export SPAWN_DEFAULT_AMI=ami-0abc123def456789

# Test new AMI
spawn launch --ami $SPAWN_DEFAULT_AMI --instance-type t3.micro --command "uname -a"
```

### Infrastructure Updates

**Lambda Functions:**
```bash
# Update DNS updater
cd lambda/dns-updater
zip -r function.zip .
aws lambda update-function-code \
  --function-name spawn-dns-updater \
  --zip-file fileb://function.zip

# Test update
aws lambda invoke --function-name spawn-dns-updater --payload '{"test": true}' response.json
```

**DynamoDB Schema:**
```bash
# Add new attribute (no downtime)
# DynamoDB is schemaless, just start writing new attribute

# Add GSI for new query pattern
aws dynamodb update-table \
  --table-name spawn-sweeps \
  --attribute-definitions AttributeName=owner,AttributeType=S \
  --global-secondary-index-updates file://gsi-update.json
```

---

## Incident Response

### Instance Failures

**Diagnosis:**
```bash
# Check instance status
spawn status i-1234567890abcdef0

# Get instance console output
aws ec2 get-console-output --instance-id i-1234567890abcdef0 --output text

# Check system log
aws logs tail /spawn/instances/i-1234567890abcdef0 --follow
```

**Resolution:**
```bash
# Restart failed instance
spawn start i-1234567890abcdef0

# Replace failed instance
spawn launch --params sweep.yaml --replace i-1234567890abcdef0
```

### Service Disruptions

**Regional Outage:**
```bash
# Check AWS Health Dashboard
aws health describe-events --filter eventTypeCategories=issue

# Failover to backup region
spawn launch --params sweep.yaml --region us-west-2
```

**Spot Interruptions:**
```bash
# Monitor interruption rate
aws ec2 describe-spot-price-history \
  --instance-types c5.4xlarge \
  --start-time $(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%S) \
  --product-descriptions "Linux/UNIX" \
  --query 'SpotPriceHistory[*].[Timestamp,SpotPrice,AvailabilityZone]'

# Switch to on-demand for critical workloads
spawn launch --params sweep.yaml --spot=false
```

### Cost Overruns

**Emergency Stop:**
```bash
# Stop all instances in sweep
spawn stop --sweep-id hyperparam-20260122-abc123

# Terminate all instances in sweep
spawn terminate --sweep-id hyperparam-20260122-abc123 --force
```

**Cost Analysis:**
```bash
# Get sweep cost breakdown
aws ce get-cost-and-usage \
  --time-period Start=2026-01-22,End=2026-01-23 \
  --granularity DAILY \
  --filter file://sweep-filter.json \
  --group-by Type=DIMENSION,Key=INSTANCE_TYPE
```

### Security Incidents

**Compromised Instance:**
```bash
# Isolate instance
aws ec2 modify-instance-attribute \
  --instance-id i-1234567890abcdef0 \
  --groups sg-isolated

# Take snapshot for forensics
aws ec2 create-snapshot \
  --volume-id vol-0abcdef1234567890 \
  --description "Forensic snapshot - compromised instance"

# Terminate instance
spawn terminate i-1234567890abcdef0
```

**Access Key Leak:**
```bash
# Deactivate compromised key
aws iam update-access-key \
  --access-key-id AKIAIOSFODNN7EXAMPLE \
  --status Inactive

# Review usage
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=AccessKeyId,AttributeValue=AKIAIOSFODNN7EXAMPLE

# Rotate keys
aws iam create-access-key --user-name spawn-user
aws iam delete-access-key --access-key-id AKIAIOSFODNN7EXAMPLE --user-name spawn-user
```

---

## Related Documentation

- [SLURM_GUIDE.md](SLURM_GUIDE.md) - Migrating from HPC clusters
- [DATA_STAGING_GUIDE.md](DATA_STAGING_GUIDE.md) - Optimizing data distribution
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) - Common issues and solutions
- [README.md](README.md) - Getting started guide

---

## Support

- **Issues:** https://github.com/spore-host/spore-host/issues
- **Discussions:** https://github.com/spore-host/spore-host/discussions
- **Email:** support@spore-host.dev
