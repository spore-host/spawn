# Auto-Scaling Job Arrays

**Status:** Production-ready (v0.20.0)
**Last Updated:** 2026-02-14

---

## Overview

Auto-scaling job arrays automatically maintain target capacity by replacing failed instances, scaling based on queue depth or CloudWatch metrics, and applying scheduled capacity changes. The system supports hybrid policies that intelligently combine multiple scaling strategies.

### Key Features

- **Automatic Health Checks**: Monitors instance health and replaces failures
- **Queue-Based Scaling**: Scales based on SQS queue depth
- **Metric-Based Scaling**: Scales based on CloudWatch metrics (CPU, memory)
- **Scheduled Scaling**: Time-based capacity changes with cron expressions
- **Multi-Queue Support**: Weighted priorities across multiple queues
- **Hybrid Policies**: Intelligently combines queue, metric, and schedule policies
- **Graceful Drain**: Waits for jobs to complete before terminating instances
- **Cross-Account**: Orchestrates EC2 instances across AWS accounts

---

## Quick Start

### 1. Launch an Autoscale Group

```bash
spawn autoscale launch \
  --name my-workers \
  --min-capacity 1 \
  --max-capacity 10 \
  --desired-capacity 3 \
  --instance-type c5.large \
  --ami ami-0c02fb55b34c1a27d \
  --subnet-id subnet-xxx \
  --security-groups sg-xxx
```

This creates a group that maintains 3 instances, replacing any that fail.

### 2. Add Queue-Based Scaling

```bash
spawn autoscale set-policy my-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.us-east-1.amazonaws.com/.../my-queue \
  --target-messages-per-instance 10
```

Now the group automatically scales based on queue depth:
- 50 messages → 5 instances
- 100 messages → 10 instances (max)
- 0 messages → 1 instance (min)

### 3. Add a Schedule

```bash
spawn autoscale add-schedule my-workers \
  --name workday-morning \
  --schedule "0 0 9 * * MON-FRI" \
  --desired-capacity 10 \
  --timezone America/New_York
```

Every weekday at 9 AM Eastern, the group scales to 10 instances.

---

## Manual Capacity Management

The simplest mode is manual capacity management where you specify the desired number of instances.

### Launch Group

```bash
spawn autoscale launch \
  --name batch-workers \
  --desired-capacity 5 \
  --min-capacity 0 \
  --max-capacity 20 \
  --instance-type t3.medium \
  --ami ami-xxx \
  --subnet-id subnet-xxx \
  --security-groups sg-xxx
```

### Update Capacity

```bash
spawn autoscale update batch-workers --desired-capacity 10
```

The autoscaler will:
1. Discover current instances (5 running)
2. Calculate needed changes (need 5 more)
3. Launch 5 new instances
4. Monitor health and replace failures

### Check Status

```bash
spawn autoscale status batch-workers
```

Output:
```
Group: asg-batch-workers-xxx
Status: active
Capacity: desired=10, min=0, max=20
Last Scale Event: 2026-02-14 10:30:00 UTC
Health Check Interval: 1m0s
```

---

## Queue-Based Scaling

Scale automatically based on SQS queue depth.

### Configuration

```bash
spawn autoscale set-policy batch-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.us-east-1.amazonaws.com/.../jobs-queue \
  --target-messages-per-instance 10 \
  --scale-up-cooldown 60 \
  --scale-down-cooldown 300
```

### How It Works

**Capacity Calculation:**
```
needed_capacity = ceil(queue_depth / target_messages_per_instance)
desired_capacity = clamp(needed_capacity, min_capacity, max_capacity)
```

**Example:**
- Queue depth: 45 messages
- Target: 10 messages/instance
- Calculation: 45 / 10 = 4.5 → 5 instances

**Cooldown Periods:**
- **Scale-up cooldown (60s)**: Prevents rapid scale-ups
- **Scale-down cooldown (300s)**: Prevents thrashing during fluctuations

### Use Cases

- **Batch processing**: Workers pulling jobs from queue
- **Event-driven workloads**: Scale with incoming work
- **Cost optimization**: Scale to zero when queue is empty

---

## Metric-Based Scaling

Scale based on CloudWatch metrics like CPU or memory utilization.

### CPU-Based Scaling

```bash
spawn autoscale set-metric-policy batch-workers \
  --metric-policy cpu \
  --target-value 70.0 \
  --metric-period 300
```

Maintains average CPU at ~70% across the fleet.

### Memory-Based Scaling

```bash
spawn autoscale set-metric-policy batch-workers \
  --metric-policy memory \
  --target-value 80.0
```

### Custom Metrics

```bash
spawn autoscale set-metric-policy batch-workers \
  --metric-policy custom \
  --metric-name "CustomMetric" \
  --metric-namespace "MyApp" \
  --target-value 100.0 \
  --metric-statistic Average
```

### Use Cases

- **Resource-bound workloads**: Scale based on actual resource usage
- **Predictable load**: Smooth scaling for gradual traffic changes
- **Cost efficiency**: Right-size capacity based on utilization

---

## Scheduled Scaling

Time-based capacity changes using cron expressions.

### Basic Schedule

```bash
spawn autoscale add-schedule batch-workers \
  --name morning-scale-up \
  --schedule "0 0 8 * * *" \
  --desired-capacity 20
```

Every day at 8:00 AM UTC, scale to 20 instances.

### Timezone Support

```bash
spawn autoscale add-schedule batch-workers \
  --name business-hours \
  --schedule "0 0 9 * * MON-FRI" \
  --desired-capacity 15 \
  --timezone America/New_York
```

Weekdays at 9 AM Eastern time.

### Multiple Schedules

```bash
# Scale up in morning
spawn autoscale add-schedule batch-workers \
  --name morning \
  --schedule "0 0 9 * * MON-FRI" \
  --desired-capacity 20 \
  --timezone America/New_York

# Scale down in evening
spawn autoscale add-schedule batch-workers \
  --name evening \
  --schedule "0 0 18 * * MON-FRI" \
  --desired-capacity 5 \
  --timezone America/New_York

# Minimal weekend capacity
spawn autoscale add-schedule batch-workers \
  --name weekend \
  --schedule "0 0 0 * * SAT" \
  --desired-capacity 1 \
  --timezone America/New_York
```

### Cron Format

6-field format: `second minute hour day month weekday`

**Examples:**
- `0 0 8 * * *` - Daily at 8:00 AM
- `0 30 14 * * MON-FRI` - Weekdays at 2:30 PM
- `0 0 0 1 * *` - First day of each month at midnight
- `0 0 */4 * * *` - Every 4 hours

### Manage Schedules

```bash
# List all schedules
spawn autoscale list-schedules batch-workers

# Remove a schedule
spawn autoscale remove-schedule batch-workers morning
```

### Use Cases

- **Business hours scaling**: High capacity during work hours
- **Batch windows**: Scale up for nightly processing
- **Cost optimization**: Minimal capacity during off-hours
- **Predictable patterns**: Recurring workload schedules

---

## Multi-Queue Scaling

Scale based on multiple SQS queues with weighted priorities.

### Equal Priority

```bash
spawn autoscale set-policy batch-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.../queue1 \
  --queue https://sqs.../queue2 \
  --target-messages-per-instance 10
```

Both queues have equal weight (1.0).

### Weighted Priority

```bash
spawn autoscale set-policy batch-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.../high-priority --queue-weight 0.7 \
  --queue https://sqs.../low-priority --queue-weight 0.3 \
  --target-messages-per-instance 10
```

**How It Works:**
```
weighted_depth = (queue1_depth × 0.7) + (queue2_depth × 0.3)
needed_capacity = ceil(weighted_depth / 10)
```

**Example:**
- High-priority queue: 100 messages × 0.7 = 70
- Low-priority queue: 100 messages × 0.3 = 30
- Weighted depth: 100 messages
- Capacity: 100 / 10 = 10 instances

### Use Cases

- **Priority queues**: High-priority gets more weight
- **Gradual migration**: Old queue (0.3) + new queue (0.7)
- **Workload distribution**: Different job types with different importance
- **Load balancing**: Distribute capacity across multiple sources

---

## Hybrid Policies

Combine multiple scaling strategies with intelligent priority handling.

### Queue + Schedule

```bash
# Set queue policy
spawn autoscale set-policy batch-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.../queue \
  --target-messages-per-instance 10

# Add schedule
spawn autoscale add-schedule batch-workers \
  --name peak-hours \
  --schedule "0 0 9-17 * * MON-FRI" \
  --desired-capacity 20 \
  --timezone America/New_York
```

**Behavior:**
- **9 AM - 5 PM weekdays**: Schedule overrides → 20 instances
- **Other times**: Queue policy applies → scales with queue depth

### Queue + Metric + Schedule

```bash
# Queue policy
spawn autoscale set-policy batch-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.../queue \
  --target-messages-per-instance 10

# Metric policy
spawn autoscale set-metric-policy batch-workers \
  --metric-policy cpu \
  --target-value 70.0

# Schedule
spawn autoscale add-schedule batch-workers \
  --name business-hours \
  --schedule "0 0 9 * * MON-FRI" \
  --desired-capacity 15
```

### Policy Priority

1. **Schedule** (highest) - Overrides all other policies during trigger window
2. **Queue + Metric** (hybrid) - Combined intelligently when schedule inactive
3. **Manual** (lowest) - Fallback when no policies configured

### Hybrid Combination Strategy

When both queue and metric policies are active (no schedule):

**Scale Up:** Take maximum (aggressive)
```
queue_desired = 10
metric_desired = 7
final_desired = max(10, 7) = 10
```
Respond quickly to either work backlog OR resource pressure.

**Scale Down:** Take maximum (conservative)
```
queue_desired = 3
metric_desired = 5
final_desired = max(3, 5) = 5
```
Only scale down when both policies agree capacity can reduce.

### Use Cases

- **Business hours + workload**: Fixed capacity during work hours, dynamic scaling off-hours
- **Work OR resource**: Scale for queue backlog OR high CPU
- **Conservative scale-down**: Only reduce when both queue empty AND CPU low
- **Complex workloads**: Different strategies for different time periods

---

## Graceful Drain

Wait for jobs to complete before terminating instances.

### Enable Drain

Drain configuration is part of the autoscale group (configured via DynamoDB):

```go
DrainConfig{
    Enabled: true,
    TimeoutSeconds: 300,        // Max wait time (5 minutes)
    CheckInterval: 30,          // Check every 30 seconds
    HeartbeatStaleAfter: 300,   // Heartbeat staleness threshold
    GracePeriodSeconds: 30,     // Wait after last job completes
}
```

### How It Works

When scaling down:
1. Mark excess instances for drain
2. Query job registry for active jobs on instance
3. Check job status == "running" and heartbeat < 5 minutes
4. If no active jobs: terminate immediately
5. If active jobs: wait and re-check (up to timeout)
6. After timeout: terminate regardless

### Job Registry Requirements

**DynamoDB Table:** `spawn-hybrid-registry`
**GSI Required:** `instance-id-index`

**Schema:**
```
job-id (String, PK)
instance-id (String, GSI Hash Key)
job-status (String: "running", "completed", "failed")
last-heartbeat (String, ISO 8601 timestamp)
start-time (String, ISO 8601 timestamp)
```

### Graceful Degradation

If registry unavailable:
- Logs warning
- Assumes no active work
- Falls back to immediate termination
- System continues functioning

### Use Cases

- **Batch jobs**: Wait for long-running jobs to finish
- **Stateful workloads**: Graceful shutdown for data consistency
- **Spot interruptions**: Checkpoint before termination
- **Safe scale-down**: Never kill active work

---

## Best Practices

### Capacity Configuration

**Min Capacity:**
- Set to 0 for pure queue-driven workloads (cost optimization)
- Set to 1+ for always-on services (availability)

**Max Capacity:**
- Set based on budget constraints
- Consider AWS service limits (vCPU, instance limits)
- Leave headroom for manual scaling if needed

**Desired Capacity:**
- Manual mode: Set explicitly
- Policy mode: Calculated automatically, respects min/max

### Cooldown Periods

**Scale-Up (60s default):**
- Shorter = More responsive to load spikes
- Longer = More cost-effective (fewer launches)

**Scale-Down (300s default):**
- Always longer than scale-up (prevents thrashing)
- Consider workload duration
- 5 minutes is good for most workloads

### Health Checks

- Default interval: 1 minute
- Checks EC2 instance state
- Optional: Job registry heartbeat checks
- Failed instances replaced automatically

### Queue Policy

**Target Messages Per Instance:**
- Too low: Expensive (many instances)
- Too high: Slow (queue backlog)
- Sweet spot: Based on job duration and throughput

**Example:**
- Job duration: 1 minute
- Throughput goal: 100 jobs/minute
- Target: 100 messages / 100 instances = 1 message/instance

### Scheduled Scaling

**Timezone Awareness:**
- Always specify timezone for business hours
- Default: UTC (can be confusing)
- Use IANA timezone names (America/New_York)

**Trigger Window:**
- 1-minute window accommodates Lambda timing jitter
- Schedule at :00, may trigger between :00:00 and :00:59

**Multiple Schedules:**
- Last scheduled action wins during overlaps
- Use non-overlapping times or min/max overrides

### Hybrid Policies

**When to Use:**
- Queue for work-driven scaling
- Metric for resource-driven scaling
- Schedule for time-driven overrides
- Combine for complex workloads

**Policy Interaction:**
- Schedule always wins (highest priority)
- Queue + metric use max() strategy
- Manual is baseline (no automatic scaling)

---

## Monitoring

### Status Command

```bash
spawn autoscale status my-workers
```

Shows:
- Current capacity (desired/min/max)
- Active policies
- Recent scaling events
- Schedule configuration

### Scaling Activity

```bash
spawn autoscale scaling-activity my-workers
```

Shows recent scaling decisions from queue policy.

### Metric Activity

```bash
spawn autoscale metric-activity my-workers
```

Shows recent metric-based scaling decisions.

### Lambda Logs

```bash
AWS_PROFILE=spore-host-infra aws logs tail \
  /aws/lambda/spawn-autoscale-orchestrator-production \
  --since 10m --follow
```

Shows:
- Reconciliation cycles (every 1 minute)
- Policy evaluations
- Capacity planning decisions
- Instance launch/terminate actions
- Errors and warnings

---

## Troubleshooting

### Instances Not Launching

**Check:**
1. EC2 service limits (vCPU quota)
2. Subnet has available IPs
3. AMI exists in region
4. Security group allows necessary traffic
5. Lambda logs for errors

**Common Issues:**
- `InsufficientInstanceCapacity`: Try different instance type or AZ
- `InvalidSubnet.NotFound`: Check subnet ID
- `UnauthorizedOperation`: Check IAM permissions

### Instances Not Scaling Down

**Check:**
1. Scale-down cooldown period (5 minutes default)
2. Min capacity setting
3. Drain configuration (may be waiting for jobs)
4. Lambda logs for capacity planning

**Common Issues:**
- Cooldown still active (wait)
- Min capacity = current capacity (increase max or decrease min)
- Active jobs preventing drain (check job registry)

### Schedule Not Triggering

**Check:**
1. Cron expression syntax (6 fields)
2. Timezone configuration
3. Schedule enabled status
4. Lambda execution logs

**Common Issues:**
- Wrong field count (need 6 fields with seconds)
- Timezone mismatch (specify explicitly)
- Schedule disabled (re-add or edit)

### Queue Policy Not Working

**Check:**
1. Queue URL correct
2. Lambda has SQS permissions
3. Target messages per instance reasonable
4. Cooldown periods
5. Min/max capacity bounds

**Debug:**
```bash
# Check queue depth
aws sqs get-queue-attributes \
  --queue-url https://sqs.../queue \
  --attribute-names ApproximateNumberOfMessages

# Check policy configuration
spawn autoscale status my-workers
```

### Cross-Account Issues

**Check:**
1. Lambda assumes correct cross-account role
2. EC2 role ARN configured correctly
3. External ID matches
4. IAM policies allow sts:AssumeRole

**Lambda logs should show:**
```
assuming cross-account role: arn:aws:iam::xxx:role/spawn-autoscale-ec2
autoscale orchestrator initialized
```

---

## Architecture

### Components

**Lambda Function:** `spawn-autoscale-orchestrator-production`
- Runs every 1 minute (EventBridge schedule)
- Reconciles all active autoscale groups
- Handles policy evaluation and capacity planning

**DynamoDB Table:** `spawn-autoscale-groups-production`
- Stores autoscale group configuration
- Tracks scaling state and history

**EC2 Instances:** (spore-host-dev account)
- Tagged with autoscale group ID
- Launched/terminated by Lambda
- Health monitored via EC2 API

**SQS Queues:** (spore-host-infra account)
- Monitored for queue depth
- Supports multi-queue with weights

**CloudWatch:** (spore-host-dev account)
- Metrics for CPU, memory, custom metrics
- Used by metric-based policies

### Cross-Account Setup

**Infrastructure Account (spore-host-infra):**
- Lambda function
- DynamoDB tables
- SQS queues (for monitoring)

**Development Account (spore-host-dev):**
- EC2 instances
- CloudWatch metrics

**IAM Role:** Lambda assumes cross-account role to launch/terminate instances in dev account.

---

## API Reference

### Launch Group

```bash
spawn autoscale launch \
  --name <group-name> \
  --desired-capacity <N> \
  --min-capacity <N> \
  --max-capacity <N> \
  --instance-type <type> \
  --ami <ami-id> \
  [--spot] \
  [--key-name <key>] \
  [--subnet-id <subnet>] \
  [--security-groups <sg1,sg2>] \
  [--iam-profile <profile>] \
  [--user-data <base64>] \
  [--tags key=value]
```

### Update Capacity

```bash
spawn autoscale update <group-name> \
  [--desired-capacity <N>] \
  [--min-capacity <N>] \
  [--max-capacity <N>]
```

### Set Queue Policy

```bash
spawn autoscale set-policy <group-name> \
  --scaling-policy queue-depth \
  --queue <url> [--queue-weight <0.0-1.0>] \
  [--queue <url2> --queue-weight <0.0-1.0>] \
  --target-messages-per-instance <N> \
  [--scale-up-cooldown <seconds>] \
  [--scale-down-cooldown <seconds>]
```

### Set Metric Policy

```bash
spawn autoscale set-metric-policy <group-name> \
  --metric-policy <cpu|memory|custom> \
  --target-value <float> \
  [--metric-period <seconds>] \
  [--metric-name <name>] \
  [--metric-namespace <namespace>] \
  [--metric-statistic <Average|Maximum|Minimum>]
```

### Add Schedule

```bash
spawn autoscale add-schedule <group-name> \
  --name <schedule-name> \
  --schedule "<cron>" \
  --desired-capacity <N> \
  [--min-capacity <N>] \
  [--max-capacity <N>] \
  [--timezone <tz>]
```

### Remove Schedule

```bash
spawn autoscale remove-schedule <group-name> <schedule-name>
```

### List Schedules

```bash
spawn autoscale list-schedules <group-name>
```

### Status

```bash
spawn autoscale status <group-name>
```

### Health

```bash
spawn autoscale health <group-name>
```

### Scaling Activity

```bash
spawn autoscale scaling-activity <group-name>
```

### Terminate Group

```bash
spawn autoscale terminate <group-name>
```

Deletes group configuration and terminates all instances.

---

## Examples

### Example 1: Simple Batch Workers

```bash
# Launch with manual capacity
spawn autoscale launch \
  --name batch-workers \
  --desired-capacity 3 \
  --min-capacity 0 \
  --max-capacity 10 \
  --instance-type t3.medium \
  --ami ami-0c02fb55b34c1a27d \
  --subnet-id subnet-xxx \
  --security-groups sg-xxx

# Add queue policy
spawn autoscale set-policy batch-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.us-east-1.amazonaws.com/.../jobs \
  --target-messages-per-instance 10

# Monitor
spawn autoscale status batch-workers
```

### Example 2: Business Hours Scaling

```bash
# Launch group
spawn autoscale launch \
  --name api-workers \
  --desired-capacity 5 \
  --min-capacity 2 \
  --max-capacity 20 \
  --instance-type c5.large \
  --ami ami-xxx \
  --subnet-id subnet-xxx \
  --security-groups sg-xxx

# Scale up for business hours
spawn autoscale add-schedule api-workers \
  --name business-hours \
  --schedule "0 0 9 * * MON-FRI" \
  --desired-capacity 15 \
  --timezone America/New_York

# Scale down for evening
spawn autoscale add-schedule api-workers \
  --name evening \
  --schedule "0 0 18 * * MON-FRI" \
  --desired-capacity 5 \
  --timezone America/New_York

# Minimal weekend
spawn autoscale add-schedule api-workers \
  --name weekend \
  --schedule "0 0 0 * * SAT" \
  --desired-capacity 2 \
  --timezone America/New_York
```

### Example 3: Priority Queues

```bash
# Launch group
spawn autoscale launch \
  --name priority-workers \
  --desired-capacity 5 \
  --min-capacity 0 \
  --max-capacity 50 \
  --instance-type c5.xlarge \
  --ami ami-xxx \
  --subnet-id subnet-xxx \
  --security-groups sg-xxx

# Multi-queue with priority
spawn autoscale set-policy priority-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.../high-priority --queue-weight 0.8 \
  --queue https://sqs.../normal-priority --queue-weight 0.2 \
  --target-messages-per-instance 5
```

### Example 4: Hybrid Policy

```bash
# Launch group
spawn autoscale launch \
  --name hybrid-workers \
  --desired-capacity 10 \
  --min-capacity 5 \
  --max-capacity 50 \
  --instance-type c5.2xlarge \
  --ami ami-xxx \
  --subnet-id subnet-xxx \
  --security-groups sg-xxx

# Queue policy for work-driven scaling
spawn autoscale set-policy hybrid-workers \
  --scaling-policy queue-depth \
  --queue https://sqs.../work-queue \
  --target-messages-per-instance 10

# Metric policy for resource-driven scaling
spawn autoscale set-metric-policy hybrid-workers \
  --metric-policy cpu \
  --target-value 70.0

# Schedule for peak hours override
spawn autoscale add-schedule hybrid-workers \
  --name peak-hours \
  --schedule "0 0 9-17 * * MON-FRI" \
  --desired-capacity 30 \
  --timezone America/New_York
```

---

## FAQ

### Q: Can I mix spot and on-demand instances?

A: Not directly in auto-scaling groups. Launch with `--spot` for all spot, or without for all on-demand. For mixed fleets, use separate groups.

### Q: What happens during Lambda coldstart?

A: Coldstarts take 70-230ms. Reconciliation continues on next cycle (1 minute). No impact on running instances.

### Q: Can I use auto-scaling with MPI clusters?

A: Not recommended. MPI requires static hostfiles and tight coupling. Use fixed-size job arrays instead.

### Q: How much does auto-scaling cost?

A: Lambda execution: $0.20/million requests (~$0.30/month). DynamoDB: ~$1-5/month. EC2 instances charged normally.

### Q: Can I pause auto-scaling temporarily?

A: Yes: `spawn autoscale pause <group>` stops reconciliation. Resume with `spawn autoscale resume <group>`.

### Q: What if I delete the Lambda function?

A: Instances keep running but won't be managed. Manually terminate or redeploy Lambda to resume management.

### Q: Can I use auto-scaling across regions?

A: No. Each group operates in a single region. Launch separate groups per region if needed.

### Q: How do I migrate from manual to queue-based?

A: Just add the queue policy: `spawn autoscale set-policy <group> --scaling-policy queue-depth ...`. Existing instances continue running, policy takes effect on next cycle.

---

## Changelog

### v0.20.0 (2026-02-14)
- ✅ Initial release: Auto-scaling job arrays production-ready
- ✅ Phase 1: Core infrastructure (health checks, reconciliation)
- ✅ Phase 2: Queue-based scaling
- ✅ Phase 3: Metric-based scaling
- ✅ Phase 4.1: Graceful drain
- ✅ Phase 4.2: Scheduled scaling
- ✅ Phase 4.3: Multi-queue support
- ✅ Phase 4.4: Hybrid policies
- ✅ Scale-down fix in capacity planner

---

## Support

- **Issues**: https://github.com/spore-host/spore-host/issues
- **Documentation**: https://github.com/spore-host/spore-host/tree/main/spawn/docs
- **Examples**: See PHASE_4_COMPLETION.md for detailed E2E test examples
