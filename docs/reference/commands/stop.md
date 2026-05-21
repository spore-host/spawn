# spawn stop

Stop (shut down) running EC2 instances without terminating them.

## Synopsis

```bash
spawn stop [instance-id-or-name]
spawn stop --array-id <array-id>
spawn stop --array-name <array-name>
```

## Description

The `stop` command stops (shuts down) EC2 instances without terminating them. Stopped instances can be restarted later with `spawn start`.

**Stop vs Terminate:**
- **Stop:** Instance persists, EBS volumes retained, can restart later
- **Terminate:** Instance destroyed, EBS volumes deleted (unless configured otherwise)

**Use cases for stop:**
- Pause workloads overnight/weekends
- Reduce costs while preserving state
- Prepare for instance type changes
- Test startup/shutdown procedures

**Cost considerations:**
- Stopped instances: No compute charges, but EBS storage charges apply
- Elastic IPs: Charged if attached to stopped instances
- Generally cheaper to terminate and recreate (use AMIs for state)

## Options

### Target Selection

**`[instance-id-or-name]`** (positional)
- Stop single instance by ID or name
- Example: `i-0abc123def456789` or `my-instance`

**`--array-id <array-id>`**
- Stop all instances in a job array by array ID
- Example: `array-20260127-abc123`

**`--array-name <array-name>`**
- Stop all instances in a job array by array name
- Example: `ml-training-sweep`

## Arguments

One of the following must be provided:
- Instance ID/name (positional argument)
- `--array-id` flag
- `--array-name` flag

## Examples

### Stop Single Instance

**By instance ID:**
```bash
spawn stop i-0abc123def456789
```

**By instance name:**
```bash
spawn stop my-dev-instance
```

**Output:**
```
Stopping instance i-0abc123def456789...
✓ Instance stopped successfully
```

### Stop Job Array

**By array ID:**
```bash
spawn stop --array-id array-20260127-abc123
```

**By array name:**
```bash
spawn stop --array-name ml-training-sweep
```

**Output:**
```
Stopping instances in array array-20260127-abc123...

✓ Stopped i-0abc123 (worker-0)
✓ Stopped i-0def456 (worker-1)
✓ Stopped i-0ghi789 (worker-2)

3 instances stopped successfully
```

### Stop and Restart Later

**Stop for weekend:**
```bash
# Friday evening
spawn stop my-dev-instance

# Monday morning
spawn start my-dev-instance
```

**With confirmation:**
```bash
# Stop instance
spawn stop i-0abc123

# Verify stopped
spawn status i-0abc123
# State: stopped

# Restart when ready
spawn start i-0abc123
```

## Use Cases

### 1. Pause Development Environment

**Scenario:** Stop dev instance overnight to save costs.

```bash
# Stop at end of day
spawn stop my-dev-instance

# Start next morning
spawn start my-dev-instance

# Cost savings:
# - No compute charges while stopped
# - Only pay for EBS storage (~$0.10/GB/month)
# - Resume work with all state preserved
```

### 2. Stop Job Array for Maintenance

**Scenario:** Pause training sweep for infrastructure maintenance.

```bash
# Stop entire array
spawn stop --array-name hyperparameter-sweep

# Perform maintenance
# ...

# Restart array
spawn start --array-name hyperparameter-sweep
```

### 3. Cost Optimization Pattern

**Scenario:** Run workloads only during business hours.

```bash
#!/bin/bash
# stop-after-hours.sh

# Stop instances tagged with schedule=business-hours
INSTANCES=$(spawn list --tag schedule=business-hours --format json | jq -r '.[].instance_id')

for INSTANCE_ID in $INSTANCES; do
  echo "Stopping $INSTANCE_ID..."
  spawn stop $INSTANCE_ID
done
```

**Schedule with cron:**
```cron
# Stop at 6 PM
0 18 * * * /usr/local/bin/stop-after-hours.sh

# Start at 8 AM
0 8 * * * spawn start --tag schedule=business-hours
```

## Stop vs Hibernate

### Stop

**Characteristics:**
- Full shutdown, like powering off
- RAM contents lost
- EBS volumes persisted
- Reboot time: ~30-60 seconds

**Best for:**
- Short-term pauses (hours to days)
- Cost savings
- State stored on disk

### Hibernate

**Characteristics:**
- Saves RAM to EBS volume
- Faster resume (~30 seconds)
- Applications continue from exact state
- Requires hibernation-enabled AMI

**Best for:**
- Instant resume needed
- Large in-memory state
- Long-running processes

**See:** [`spawn hibernate`](hibernate.md) for hibernation

## State Transitions

```
running → stopping → stopped → pending → running
   ↓                                        ↑
   └────────── spawn stop ──────────────────┘
                                            │
                                    spawn start
```

**States:**
1. **running** - Instance operating normally
2. **stopping** - Shutdown in progress
3. **stopped** - Fully stopped
4. **pending** - Starting up
5. **running** - Operational again

## Stopped Instance Behavior

### What Persists

**Retained:**
- EBS root volume
- Attached EBS data volumes
- Instance metadata (ID, type, tags)
- Security groups
- IAM role
- Key pair association

**Lost:**
- RAM contents
- Instance store volumes
- Public IP address (unless Elastic IP)
- Running processes
- Network connections

### What Changes

**IP addresses:**
- Public IP: Released (reassigned on start)
- Private IP: Retained (same subnet)
- Elastic IP: Retained (if attached)

**Networking:**
- DNS records: Deregistered by spored
- Security group rules: Unchanged
- VPC placement: Same

## Costs While Stopped

### Charges That Continue

**EBS volumes:**
- Root volume: $0.10/GB/month (gp3)
- Data volumes: Charged at provisioned size
- Snapshots: $0.05/GB/month

**Elastic IPs:**
- Attached to stopped instance: $0.005/hour
- Not attached: Free (if < 1 per running instance)

**Other resources:**
- EFS mounts: Charged normally
- S3 buckets: Charged normally
- CloudWatch Logs: Charged normally

### Charges That Stop

**Compute:**
- Instance hours: $0/hour (no charge)
- vCPU usage: $0
- Memory usage: $0

**Network:**
- Data transfer: $0 (no traffic)
- VPC endpoints: $0 (if hourly)

### Cost Comparison

**Example: m7i.xlarge (4 vCPU, 16 GB) with 50 GB EBS**

| State | Cost/Hour | Cost/Day | Cost/Month |
|-------|-----------|----------|------------|
| Running | $0.1920 | $4.61 | $138.24 |
| Stopped | $0.0069 | $0.17 | $5.00 |
| **Savings** | **96%** | **96%** | **96%** |

**Better alternative:**
- Terminate and recreate from AMI
- AMI snapshot: $2.50/month (50 GB)
- No stopped instance charges
- Launch fresh when needed

## Limitations

### Cannot Stop

**Spot instances:**
- Spot instances cannot be stopped
- Can only be terminated
- Use terminate/relaunch pattern instead

**Instance store-backed:**
- Instances with instance store root volumes
- Cannot be stopped (stop = terminate)
- Use EBS-backed instances for stop/start

### Stop Duration Limits

**No hard limits:**
- Can remain stopped indefinitely
- However, consider:
  - Accumulating EBS storage costs
  - Potential for underlying hardware issues
  - Better to terminate and recreate for long-term

## Permissions

### Required IAM Permissions

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeInstances",
      "ec2:StopInstances"
    ],
    "Resource": "*"
  }]
}
```

## Troubleshooting

### Instance Won't Stop

**Problem:** Instance remains in "stopping" state.

**Causes:**
1. Application preventing shutdown
2. Filesystem sync taking long time
3. Hardware issue

**Solution:**
```bash
# Wait 5-10 minutes
spawn status i-0abc123

# Force stop if needed (via AWS console or CLI)
aws ec2 stop-instances --instance-ids i-0abc123 --force

# Or terminate if stuck
spawn cancel i-0abc123
```

### Lost Public IP After Start

**Problem:** Different public IP after starting.

**Expected behavior:**
- Public IPs are dynamic by default
- Released on stop, new one assigned on start

**Solution:**
```bash
# Use Elastic IP for static IP
EIP=$(aws ec2 allocate-address --domain vpc --query 'AllocationId' --output text)

aws ec2 associate-address \
  --instance-id i-0abc123 \
  --allocation-id $EIP

# Now IP persists across stop/start
```

### High Costs While Stopped

**Problem:** Still incurring charges while stopped.

**Cause:** EBS volume costs accumulating.

**Solution:**
```bash
# Check EBS volume sizes
aws ec2 describe-volumes \
  --filters "Name=attachment.instance-id,Values=i-0abc123"

# Consider terminating instead of stopping
spawn cancel i-0abc123

# Create AMI before terminating if state needed
spawn create-ami i-0abc123 --name my-instance-snapshot
```

## Related Commands

- **[spawn start](start.md)** - Start stopped instances
- **[spawn hibernate](hibernate.md)** - Hibernate instances (saves RAM)
- **[spawn cancel](cancel.md)** - Terminate instances
- **[spawn status](status.md)** - Check instance state
- **[spawn list](list.md)** - List instances and their states

## See Also

- [How-To: Cost Optimization](../../how-to/cost-optimization.md) - Stop vs terminate analysis
- [Tutorial 6: Cost Management](../../tutorials/06-cost-management.md) - Cost patterns
- [AWS EC2 Instance Lifecycle](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-lifecycle.html)
