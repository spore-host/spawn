# Hybrid Compute Guide

Complete guide to using spawn's hybrid compute system for local and cloud workloads.

## Overview

Spawn's hybrid compute system enables workloads to run on local systems (workstations, lab instruments, on-premise servers) with the ability to automatically or manually burst to AWS cloud when additional capacity is needed.

**Key Features:**
- Run spored on local systems without EC2 dependency
- Mixed local + cloud job arrays and pipelines
- Automatic cloud bursting based on queue depth
- Cost budget enforcement
- Unified peer discovery via DynamoDB
- Graceful degradation (cloud-only features disabled locally)

## Architecture

```
LOCAL ENVIRONMENT                    CLOUD ENVIRONMENT (AWS)
┌────────────────────┐              ┌────────────────────┐
│ Local Orchestrator │◄────────────►│ Cloud Orchestrator │
│ (spawn-orchestrator)│  Coordinate  │ (Lambda)          │
└─────────┬──────────┘              └─────────┬──────────┘
          │                                    │
          │ DynamoDB Peer Registry             │ EC2 API
          │                                    │
  ┌───────▼─────────┐                  ┌──────▼────────┐
  │  Local spored   │                  │ Cloud spored  │
  │  Instances      │                  │ Instances     │
  │                 │                  │               │
  │  - Workstation  │                  │  - EC2 worker │
  │  - Lab server   │                  │  - EC2 worker │
  │  - Instrument   │                  │  - EC2 worker │
  └─────────┬───────┘                  └───────┬───────┘
            │                                   │
            └───────────┬───────────────────────┘
                        │
                ┌───────▼────────┐
                │ Shared Services│
                │  - S3          │
                │  - DynamoDB    │
                │  - SQS         │
                └────────────────┘
```

## Quick Start

### 1. Local-Only Mode

Run spored on a local system without cloud bursting:

```bash
# Create local config
cat > /etc/spawn/local.yaml <<EOF
instance_id: workstation-01
region: local
ttl: 8h
idle_timeout: 1h
on_complete: exit

job_array:
  id: my-pipeline
  index: 0
EOF

# Run queue locally
spored run-queue s3://my-bucket/queue.yaml
```

### 2. Manual Cloud Burst

Launch cloud instances to join local job array:

```bash
# Ensure DynamoDB registry table exists
spawn-orchestrator ensure-table

# Start local instance (will register with DynamoDB)
spored run-queue s3://my-bucket/large-queue.yaml &

# Burst 5 cloud instances to help
spawn burst \
  --count 5 \
  --instance-type c5.4xlarge \
  --job-array-id my-pipeline \
  --spot

# Monitor hybrid job array
spawn status --job-array-id my-pipeline
```

### 3. Automatic Cloud Burst

Let orchestrator automatically scale based on queue depth:

```bash
# Create orchestrator config
cat > orchestrator-config.yaml <<EOF
job_array_id: my-pipeline
queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/my-queue
region: us-east-1

burst_policy:
  mode: auto
  queue_depth_threshold: 100
  max_cloud_instances: 20
  cost_budget: 10.0
  instance_type: c5.4xlarge
  ami: ami-12345  # Your spawn AMI
  spot: true
EOF

# Start orchestrator daemon
spawn-orchestrator run orchestrator-config.yaml

# Submit work - orchestrator auto-scales
spawn queue submit --file huge-queue.yaml
```

## Configuration

### Local Configuration (`/etc/spawn/local.yaml`)

```yaml
# Instance identity
instance_id: workstation-01  # or "auto" to use hostname
region: local
account_id: "my-organization"
public_ip: auto  # query ifconfig.me, or explicit IP
private_ip: 192.168.1.100

# Lifecycle configuration
ttl: 8h
idle_timeout: 1h
hibernate_on_idle: false
idle_cpu_percent: 5.0
on_complete: exit  # "exit" instead of "terminate"
completion_file: /tmp/SPAWN_COMPLETE
completion_delay: 30s

# DNS configuration (optional)
dns:
  enabled: false  # Local instances skip DNS registration
  name: workstation-01
  domain: local.mydomain.com

# Job array configuration
job_array:
  id: genomics-pipeline
  name: sequencing
  index: 0  # This instance is index 0 in the array
```

**Environment Variable Alternative:**

```bash
export SPAWN_PROVIDER=local
export SPAWN_INSTANCE_ID=workstation-01
export SPAWN_REGION=local
export SPAWN_TTL=8h
export SPAWN_IDLE_TIMEOUT=1h
export SPAWN_JOB_ARRAY_ID=genomics-pipeline
```

### Orchestrator Configuration

```yaml
# Job array to coordinate
job_array_id: my-pipeline

# SQS queue URL to monitor
queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/my-queue

# AWS region
region: us-east-1

# Burst policy
burst_policy:
  # Mode: "manual" (no auto-burst), "auto" (automatic)
  mode: auto

  # Burst if queue depth exceeds this threshold
  queue_depth_threshold: 100

  # Maximum cloud instances to launch
  max_cloud_instances: 50

  # Minimum cloud instances to keep warm (0 = scale to zero)
  min_cloud_instances: 0

  # Maximum cost per hour ($) - stop bursting if exceeded
  cost_budget: 10.0

  # Wait this long before terminating idle instances
  scale_down_delay: 5m

  # How often to check queue depth
  check_interval: 1m

  # Instance configuration
  instance_type: c5.4xlarge
  ami: ami-0abcdef1234567890  # Your spawn AMI
  spot: true                  # Use Spot instances for cost savings

  # Optional: SSH key, networking
  key_name: my-key
  subnet_id: subnet-abc123
  security_groups:
    - sg-abc123
```

## Use Cases

### Scientific Computing Lab

**Scenario:** Lab has local workstations and instruments generating data. Most work runs locally, but large analyses burst to cloud.

```yaml
# Instrument config (always-on)
instance_id: sequencer-01
region: local
ttl: 0  # Never terminate
idle_timeout: 0
on_complete: ""  # Don't exit after jobs

job_array:
  id: genomics-pipeline
  index: 0

# Orchestrator config
burst_policy:
  mode: auto
  queue_depth_threshold: 50
  max_cloud_instances: 20
  instance_type: r5.4xlarge  # Memory-optimized for genomics
  cost_budget: 20.0
```

**Workflow:**
1. Instrument generates data → uploads to S3 → enqueues job
2. Local workstations process small jobs
3. When queue > 50 jobs, orchestrator bursts to cloud
4. Cloud instances process overflow work
5. Cloud scales down when queue drains

### CI/CD Build Farm

**Scenario:** On-premise build servers handle most builds. Burst to cloud for release builds or CI spikes.

```yaml
# Build server config
instance_id: build-server-01
region: local
ttl: 0
idle_timeout: 2h  # Terminate if idle for 2 hours

job_array:
  id: ci-builds
  index: 0

# Orchestrator config
burst_policy:
  mode: auto
  queue_depth_threshold: 20
  max_cloud_instances: 10
  instance_type: c5.2xlarge
  cost_budget: 5.0
```

### Development Workflow

**Scenario:** Developer runs jobs locally during development, bursts to cloud for large test runs.

```bash
# Run locally during development
spored run-queue s3://dev-bucket/test-queue.yaml

# Burst for large test
spawn burst --count 5 --instance-type c5.xlarge --job-array-id test-run
```

## DynamoDB Peer Registry

The hybrid compute system uses DynamoDB as a central registry for all instances (local + cloud).

### Table Schema

**Table Name:** `spawn-hybrid-registry`

**Keys:**
- Partition Key: `job_array_id` (String)
- Sort Key: `instance_id` (String)

**Attributes:**
- `job_array_id` - Job array identifier
- `instance_id` - Instance identifier (e.g., "local-workstation-01" or "i-abc123")
- `index` - Instance index in job array
- `provider` - "local" or "ec2"
- `ip_address` - Public IP address
- `private_ip` - Private IP address
- `region` - AWS region or "local"
- `registered_at` - Unix timestamp when registered
- `last_heartbeat` - Unix timestamp of last heartbeat
- `expires_at` - Unix timestamp when entry expires (TTL)
- `status` - "pending", "running", "completed"

### Heartbeat Mechanism

- Instances send heartbeat every **30 seconds**
- Entries expire after **1 hour** without heartbeat
- Expired entries filtered during peer discovery
- Automatic cleanup via DynamoDB TTL

### Creating the Table

```bash
# Manually create table
aws dynamodb create-table \
  --table-name spawn-hybrid-registry \
  --attribute-definitions \
    AttributeName=job_array_id,AttributeType=S \
    AttributeName=instance_id,AttributeType=S \
  --key-schema \
    AttributeName=job_array_id,KeyType=HASH \
    AttributeName=instance_id,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST

# Or use orchestrator
spawn-orchestrator ensure-table
```

## Cost Management

### Budget Enforcement

The orchestrator enforces cost budgets to prevent runaway spending:

```yaml
burst_policy:
  cost_budget: 10.0  # Max $10/hour
```

**How it works:**
1. Orchestrator tracks managed instances
2. Calculates cost per hour based on instance type
3. Stops bursting when budget reached
4. Resumes when instances terminate and cost drops

### Cost Calculation

Built-in pricing (US East 1, approximate):

| Instance Type | On-Demand | Spot (70% savings) |
|--------------|-----------|---------------------|
| t3.micro     | $0.0104/hr | $0.0031/hr |
| t3.small     | $0.0208/hr | $0.0062/hr |
| c5.large     | $0.085/hr  | $0.026/hr |
| c5.4xlarge   | $0.68/hr   | $0.20/hr |
| m5.2xlarge   | $0.384/hr  | $0.115/hr |

**Example:**
- Budget: $5/hour
- Instance type: c5.4xlarge Spot ($0.20/hr)
- Max instances: $5 / $0.20 = **25 instances**

### Cost Optimization Tips

1. **Use Spot instances** - 70% cheaper than on-demand
2. **Set aggressive scale-down delay** - Terminate idle instances quickly
3. **Right-size instance types** - Don't over-provision
4. **Use min_cloud_instances: 0** - Scale to zero when queue empty
5. **Monitor costs** - Check orchestrator logs for current spend

## Scaling Policies

### Scale-Up Logic

Orchestrator launches instances when:
1. Queue depth > `queue_depth_threshold`
2. Current capacity < queue depth
3. Current cloud instances < `max_cloud_instances`
4. Current cost < `cost_budget`

**Calculation:**
```
needed_total = (queue_depth / 10) + 1
needed_new = needed_total - (local_count + cloud_count)
to_launch = min(needed_new, max_cloud_instances - current_cloud, 10)
```

Assumes each instance can handle ~10 jobs concurrently.

### Scale-Down Logic

Orchestrator terminates instances when:
1. Queue depth < `queue_depth_threshold / 2`
2. Current cloud instances > `min_cloud_instances`
3. Instance idle for > `scale_down_delay`

**Calculation:**
```
needed_instances = (queue_depth / 10) + 1
excess = cloud_count - needed_instances
to_terminate = min(excess, cloud_count - min_cloud_instances, 5)
```

Terminates up to 5 instances per cycle.

### Tuning Parameters

**High-Throughput:**
```yaml
queue_depth_threshold: 200
max_cloud_instances: 100
instance_type: c5.9xlarge
scale_down_delay: 2m
```

**Cost-Optimized:**
```yaml
queue_depth_threshold: 50
max_cloud_instances: 10
instance_type: t3.large
scale_down_delay: 10m
min_cloud_instances: 0
```

**Low-Latency (Warm Pool):**
```yaml
min_cloud_instances: 5
queue_depth_threshold: 10
scale_down_delay: 30m
```

## Deployment

### Systemd Service (Orchestrator)

```ini
[Unit]
Description=Spawn Orchestrator - Automatic cloud burst daemon
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/spawn-orchestrator run /etc/spawn/orchestrator-config.yaml
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

# Environment
Environment="AWS_REGION=us-east-1"
# Environment="AWS_PROFILE=default"  # Uncomment if using named profile

[Install]
WantedBy=multi-user.target
```

**Install:**
```bash
sudo cp spawn-orchestrator /usr/local/bin/
sudo cp orchestrator-config.yaml /etc/spawn/
sudo cp spawn-orchestrator.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable spawn-orchestrator
sudo systemctl start spawn-orchestrator
```

### Systemd Service (Local spored)

```ini
[Unit]
Description=Spored - Local spawn daemon
After=network.target

[Service]
Type=simple
User=root
Environment="SPAWN_CONFIG=/etc/spawn/local.yaml"
ExecStart=/usr/local/bin/spored run-queue s3://my-bucket/queue.yaml
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

## Monitoring

### Orchestrator Logs

```bash
# View orchestrator status
spawn-orchestrator status

# View logs
journalctl -u spawn-orchestrator -f

# Check current burst state
tail -f /var/log/spawn-orchestrator.log
```

**Log Output:**
```
2026-01-30 10:00:00 Orchestrator started (mode: auto, job_array: my-pipeline)
2026-01-30 10:01:00 Queue depth: 150, Local: 2, Cloud: 5, Total: 7
2026-01-30 10:01:00 Scaling up: launching 8 instances
2026-01-30 10:01:05 Launched instance: i-abc123 (cost: $0.680/hour)
2026-01-30 10:05:00 Queue depth: 50, Local: 2, Cloud: 13, Total: 15
2026-01-30 10:10:00 Queue depth: 20, Local: 2, Cloud: 13, Total: 15
2026-01-30 10:10:00 Scaling down: terminating 5 instances
```

### DynamoDB Registry Query

```bash
# List all instances in job array
aws dynamodb query \
  --table-name spawn-hybrid-registry \
  --key-condition-expression "job_array_id = :id" \
  --expression-attribute-values '{":id":{"S":"my-pipeline"}}' \
  --query 'Items[*].[instance_id.S, provider.S, status.S]' \
  --output table
```

### CloudWatch Metrics

Create custom CloudWatch metrics for monitoring:

```bash
# Publish queue depth metric
aws cloudwatch put-metric-data \
  --namespace Spawn/Hybrid \
  --metric-name QueueDepth \
  --value 150 \
  --dimensions JobArrayId=my-pipeline

# Publish cost metric
aws cloudwatch put-metric-data \
  --namespace Spawn/Hybrid \
  --metric-name HourlyCost \
  --value 5.50 \
  --dimensions JobArrayId=my-pipeline
```

## Troubleshooting

### Issue: Local instances not discovering cloud instances

**Symptoms:**
- `spawn status` shows only local or only cloud instances
- Job array not coordinating work

**Solutions:**
1. Check DynamoDB table exists: `aws dynamodb describe-table --table-name spawn-hybrid-registry`
2. Verify AWS credentials on local system: `aws sts get-caller-identity`
3. Check local config has `job_array.id` set
4. Verify instances registered: Query DynamoDB table
5. Check firewall allows DynamoDB access (port 443 HTTPS)

### Issue: Orchestrator not bursting

**Symptoms:**
- Queue depth high but no cloud instances launching
- Logs show "Manual mode - no automatic bursting"

**Solutions:**
1. Check `burst_policy.mode: auto` in config
2. Verify queue_depth > `queue_depth_threshold`
3. Check budget not exceeded: Look for "Budget limit reached" in logs
4. Verify AWS credentials: `aws ec2 describe-instances --max-items 1`
5. Check AMI ID is valid in region
6. Verify subnet and security groups exist

### Issue: Instances not terminating

**Symptoms:**
- Cloud instances stay running after queue empty
- Cost continues accumulating

**Solutions:**
1. Check `scale_down_delay` - May need to wait longer
2. Verify `min_cloud_instances` not preventing termination
3. Check instances have `spawn:auto-burst=true` tag
4. Manually terminate: `spawn burst --terminate --job-array-id X`

### Issue: "Provider detection failed"

**Symptoms:**
- Error: "failed to get instance identity (not EC2?)"
- spored won't start

**Solutions:**
1. Create local config file: `/etc/spawn/local.yaml`
2. Set `SPAWN_CONFIG` environment variable
3. Verify config file valid YAML
4. Check file permissions: `chmod 644 /etc/spawn/local.yaml`

### Issue: DynamoDB access denied

**Symptoms:**
- Error: "User: arn:aws:iam::123456789012:user/alice is not authorized to perform: dynamodb:Query"

**Solutions:**
1. Attach DynamoDB policy to IAM user/role:
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:Query",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:DeleteItem",
        "dynamodb:GetItem"
      ],
      "Resource": "arn:aws:dynamodb:*:*:table/spawn-hybrid-registry"
    }
  ]
}
```
2. For EC2 instances, attach IAM role with DynamoDB permissions
3. For local systems, use IAM user with programmatic access

## Security

### IAM Permissions

**Local Instances:**
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::my-bucket/*",
        "arn:aws:s3:::my-bucket"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:Query",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:DeleteItem"
      ],
      "Resource": "arn:aws:dynamodb:*:*:table/spawn-hybrid-registry"
    },
    {
      "Effect": "Allow",
      "Action": [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:SendMessage"
      ],
      "Resource": "arn:aws:sqs:*:*:my-queue"
    }
  ]
}
```

**Orchestrator:**
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:Query",
        "dynamodb:Scan"
      ],
      "Resource": "arn:aws:dynamodb:*:*:table/spawn-hybrid-registry"
    },
    {
      "Effect": "Allow",
      "Action": [
        "sqs:GetQueueAttributes"
      ],
      "Resource": "arn:aws:sqs:*:*:*"
    }
  ]
}
```

### Network Security

1. **DynamoDB Access**: Requires HTTPS (port 443) outbound
2. **S3 Access**: Requires HTTPS (port 443) outbound
3. **SQS Access**: Requires HTTPS (port 443) outbound
4. **EC2 Instances**: Configure security groups for SSH (if needed)

### Best Practices

1. **Use IAM roles for EC2 instances** - Avoid hardcoded credentials
2. **Use IAM users with MFA for local systems** - Protect AWS access
3. **Rotate credentials regularly** - Use AWS Secrets Manager
4. **Encrypt S3 data at rest** - Enable S3 bucket encryption
5. **Use VPC endpoints** - Avoid internet traffic for AWS services
6. **Audit with CloudTrail** - Track all API calls

## Examples

See [examples/hybrid/](../examples/hybrid/) for complete working examples:

- `local-workstation/` - Run spored on local workstation
- `auto-burst/` - Automatic cloud bursting
- `manual-burst/` - Manual cloud bursting
- `cost-optimized/` - Cost-optimized configuration
- `genomics-pipeline/` - Real-world genomics workflow

## Related Documentation

- [Queue System Guide](QUEUE_GUIDE.md) - Queue execution details
- [Pipeline Guide](PIPELINE_GUIDE.md) - Multi-stage pipelines
- [Cloud Economics](CLOUD_ECONOMICS.md) - Cost optimization
- [Testing Guide](TESTING.md) - Testing hybrid compute

## FAQ

**Q: Can I run hybrid compute without AWS?**
A: No, hybrid compute requires DynamoDB for peer registry and S3 for queue/results storage. You must have AWS credentials.

**Q: What if my local system has no internet access?**
A: Hybrid compute requires internet access to reach AWS services (DynamoDB, S3, SQS). For air-gapped environments, use local-only mode without job arrays.

**Q: Can I mix multiple local sites with cloud?**
A: Yes! Each site can have local instances that all coordinate via DynamoDB. For example, Lab A + Lab B + Cloud all in one job array.

**Q: How much does hybrid compute cost?**
A: DynamoDB costs ~$0.25/million requests. For typical workloads with 100 instances sending heartbeats every 30s, this is ~$20/month. S3 and SQS costs depend on usage.

**Q: Can I use Azure or GCP instead of AWS?**
A: Not currently. Hybrid compute is AWS-only. Support for other clouds could be added via provider interface.

**Q: What happens if DynamoDB goes down?**
A: Instances can't discover peers but can continue processing assigned work. Orchestrator can't scale. Service should resume when DynamoDB recovers.

**Q: Can I use this for real-time workloads?**
A: Hybrid compute is designed for batch workloads. There's ~30s latency for peer discovery (heartbeat interval). Not suitable for sub-second coordination.

## Support

- **Issues**: https://github.com/spore-host/spore-host/issues
- **Discussions**: https://github.com/spore-host/spore-host/discussions
- **Email**: support@spore-host.dev
