# spawn start

Start stopped or hibernated EC2 instances.

## Synopsis

```bash
spawn start [instance-id-or-name]
spawn start --array-id <array-id>
spawn start --array-name <array-name>
```

## Description

The `start` command starts stopped or hibernated EC2 instances, bringing them back to the running state.

**Start behavior depends on instance state:**
- **Stopped:** Normal boot, processes restart from scratch (~60-120s)
- **Hibernated:** Fast resume, processes continue from saved state (~30-60s)

**Use cases:**
- Resume work after stopping instances
- Restart instances after planned maintenance
- Resume hibernated instances with preserved state
- Bring up instances stopped by schedule

## Options

### Target Selection

**`[instance-id-or-name]`** (positional)
- Start single instance by ID or name
- Example: `i-0abc123def456789` or `my-instance`

**`--array-id <array-id>`**
- Start all instances in a job array by array ID
- Example: `array-20260127-abc123`

**`--array-name <array-name>`**
- Start all instances in a job array by array name
- Example: `ml-training-sweep`

## Arguments

One of the following must be provided:
- Instance ID/name (positional argument)
- `--array-id` flag
- `--array-name` flag

## Examples

### Start Single Instance

**By instance ID:**
```bash
spawn start i-0abc123def456789
```

**By instance name:**
```bash
spawn start my-dev-instance
```

**Output:**
```
Starting instance i-0abc123def456789...
✓ Instance started successfully (state: running)
Time: 42s
```

### Start Stopped Instance

**Normal boot from stopped state:**
```bash
# Start instance
spawn start my-instance

# Wait for SSH
spawn status my-instance --wait-for-ssh

# Connect when ready
spawn connect my-instance
```

### Start Hibernated Instance

**Fast resume from hibernation:**
```bash
# Start hibernated instance
spawn start my-ml-training

# Output:
# Resuming from hibernation...
# Restoring 32 GB RAM from EBS...
# ✓ Instance started (28s)
# All processes resumed from saved state
```

### Start Job Array

**By array ID:**
```bash
spawn start --array-id array-20260127-abc123
```

**By array name:**
```bash
spawn start --array-name hyperparameter-sweep
```

**Output:**
```
Starting instances in array array-20260127-abc123...

✓ Started i-0abc123 (worker-0) - 45s
✓ Started i-0def456 (worker-1) - 48s
✓ Started i-0ghi789 (worker-2) - 42s

3 instances started successfully
Average start time: 45s
```

## Use Cases

### 1. Resume Development Work

**Scenario:** Stopped dev instance overnight, resume in morning.

```bash
# Morning: Start instance
spawn start my-dev-instance

# Wait for ready
spawn status my-dev-instance --wait-for-ssh

# Connect and continue work
spawn connect my-dev-instance
```

### 2. Scheduled Start/Stop

**Scenario:** Automated start at beginning of business hours.

**Cron job:**
```cron
# Start instances at 8 AM
0 8 * * 1-5 spawn start --tag schedule=business-hours

# Stop instances at 6 PM
0 18 * * 1-5 spawn stop --tag schedule=business-hours
```

**Script:**
```bash
#!/bin/bash
# start-business-hours.sh

echo "Starting business-hours instances..."

INSTANCES=$(spawn list \
  --tag schedule=business-hours \
  --state stopped \
  --format json | jq -r '.[].instance_id')

for INSTANCE_ID in $INSTANCES; do
  echo "Starting $INSTANCE_ID..."
  spawn start $INSTANCE_ID
done

echo "All instances started"
```

### 3. Resume After Maintenance

**Scenario:** Stopped instances for maintenance, now restarting.

```bash
# Get stopped instances
STOPPED=$(spawn list --state stopped --format json | jq -r '.[].instance_id')

# Start all stopped instances
for INSTANCE in $STOPPED; do
  spawn start $INSTANCE

  # Wait a bit between starts to avoid API throttling
  sleep 2
done

# Verify all running
spawn list --state running
```

### 4. Resume Hibernated ML Training

**Scenario:** Hibernated training overnight, resume with RAM intact.

```bash
# Start hibernated instance
spawn start ml-training

# Training resumes from exact point
# No checkpoint reload needed
# All gradients still in memory
```

## Start Time Comparison

### Normal Start (Stopped)

**Timeline:**
```
0s    Request start
↓
2s    Instance pending
↓
30s   Instance booting (AMI loading)
↓
45s   OS booting
↓
60s   Services starting
↓
75s   SSH available
↓
90s   Application ready
```

**Total:** 60-120 seconds

### Fast Resume (Hibernated)

**Timeline:**
```
0s    Request start
↓
2s    Instance pending
↓
10s   RAM loading from EBS
↓
25s   Processes resuming
↓
30s   SSH available
↓
35s   Application operational
```

**Total:** 30-60 seconds

## State Transitions

```
stopped/hibernated → pending → running
         ↑                        ↓
         └───── spawn stop/hibernate
```

**States:**
1. **stopped** - Instance fully stopped
2. **hibernated** - Instance stopped with RAM saved
3. **pending** - Start in progress
4. **running** - Instance operational

## What Happens on Start

### From Stopped State

**Boot sequence:**
1. Instance allocated to physical host
2. BIOS/UEFI initialization
3. Bootloader loads kernel
4. Kernel initializes
5. systemd starts services
6. spored agent starts
7. DNS registration (if enabled)
8. User data script execution (if present)

**Changes:**
- New public IP (unless Elastic IP)
- Private IP retained
- All EBS volumes reattached
- Processes start from scratch

**Time:** 60-120 seconds

### From Hibernated State

**Resume sequence:**
1. Instance allocated to physical host
2. BIOS/UEFI initialization
3. Kernel loads hibernation image from EBS
4. RAM contents restored
5. Processes resume execution
6. DNS registration (if enabled)

**Preserved:**
- RAM contents
- Running processes
- Open file handles
- Network connections (may timeout)

**Time:** 30-60 seconds

## Monitoring Start Progress

### Wait for SSH

```bash
# Start instance
spawn start my-instance

# Wait until SSH available
spawn status my-instance --wait-for-ssh

# Connect when ready
spawn connect my-instance
```

### Watch Status

```bash
# Start instance
spawn start i-0abc123 &

# Monitor status in real-time
watch -n 2 'spawn status i-0abc123'

# Or poll until running
while [ "$(spawn status i-0abc123 --json | jq -r '.state')" != "running" ]; do
  echo "Waiting for instance to start..."
  sleep 5
done

echo "Instance is running!"
```

### Check Start Time

```bash
# Start with timing
TIME_START=$(date +%s)
spawn start i-0abc123
spawn status i-0abc123 --wait-for-ssh
TIME_END=$(date +%s)

DURATION=$((TIME_END - TIME_START))
echo "Instance started in ${DURATION} seconds"
```

## Cost Implications

### When Charges Resume

**Compute charges:**
- Resume when instance state = running
- Billed per-second (minimum 60 seconds)
- Full instance price applies

**Network charges:**
- Data transfer out charged normally
- VPC endpoint charges (if hourly) resume

**Example cost resumption:**
```
Stopped:    $0.0055/hour (EBS only)
            ↓
Start:      State changes to "running"
            ↓
Running:    $0.3840/hour (full instance price)
```

### Cost Optimization

**For frequent start/stop:**
- Consider Savings Plan (up to 72% savings)
- Use Elastic IPs to retain IP ($0.005/hour when stopped)
- Delete large EBS volumes if not needed

**For infrequent use:**
- Terminate instead of stop
- Create AMI before terminating
- Launch fresh when needed

## Batch Operations

### Start Multiple Instances

**By tag:**
```bash
# Start all instances with specific tag
INSTANCES=$(spawn list --tag project=ml --state stopped --format json | jq -r '.[].instance_id')

for INSTANCE_ID in $INSTANCES; do
  spawn start $INSTANCE_ID &
done

wait
echo "All instances started"
```

**Parallel start with limits:**
```bash
#!/bin/bash
# start-instances.sh

MAX_PARALLEL=10

INSTANCES=$(spawn list --state stopped --format json | jq -r '.[].instance_id')

echo "$INSTANCES" | xargs -n 1 -P $MAX_PARALLEL -I {} sh -c '
  echo "Starting {}..."
  spawn start {}
'
```

## Limitations

### Cannot Start

**Terminated instances:**
- Terminated instances cannot be started
- Use `spawn launch` to create new instance

**Spot instances:**
- Spot instances cannot be stopped/started
- Can only be terminated

**Unsupported states:**
- Instance already running: No-op
- Instance terminating: Error
- Instance in transition (pending/stopping): Must wait

## Permissions

### Required IAM Permissions

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeInstances",
      "ec2:StartInstances"
    ],
    "Resource": "*"
  }]
}
```

## Troubleshooting

### Instance Won't Start

**Problem:** Instance stuck in "pending" state.

**Causes:**
1. Insufficient capacity in availability zone
2. Instance type not available
3. Account limit reached
4. Underlying hardware issue

**Solution:**
```bash
# Wait 5-10 minutes
spawn status i-0abc123

# Check system logs
aws ec2 get-console-output --instance-id i-0abc123

# If still stuck, stop and start again
spawn stop i-0abc123
spawn start i-0abc123

# Or terminate and recreate
spawn cancel i-0abc123
spawn launch --ami <same-ami> --instance-type <same-type>
```

### Slow Start Time

**Problem:** Start taking > 2 minutes.

**Expected times:**
- Stopped instance: 60-120s normal
- Hibernated instance: 30-60s normal
- > 3 minutes: investigate

**Causes:**
1. Large user data script
2. Many attached EBS volumes
3. Slow AMI
4. Network issues

**Debugging:**
```bash
# Check console output
aws ec2 get-console-output --instance-id i-0abc123

# Check user data execution time
spawn connect i-0abc123 -c "journalctl -u cloud-final"
```

### Public IP Changed

**Problem:** Different public IP after start.

**Expected behavior:**
- Public IPs are dynamic by default
- New IP assigned on each start

**Solution:**
```bash
# Use Elastic IP for static IP
EIP=$(aws ec2 allocate-address --domain vpc --query 'AllocationId' --output text)

aws ec2 associate-address \
  --instance-id i-0abc123 \
  --allocation-id $EIP

# IP now persists across stop/start
```

### Application Errors After Start

**Problem:** Application fails after starting stopped instance.

**Common causes:**
1. Changed public IP (hardcoded in config)
2. Expired credentials/sessions
3. Lost temporary files
4. Network connections timed out

**Solution:**
```bash
# Use hibernation instead (preserves state)
spawn launch --hibernation-enabled ...

# Or design application for graceful restart
```

## Related Commands

- **[spawn stop](stop.md)** - Stop running instances
- **[spawn hibernate](hibernate.md)** - Hibernate instances (saves RAM)
- **[spawn cancel](cancel.md)** - Terminate instances
- **[spawn status](status.md)** - Check instance state
- **[spawn launch](launch.md)** - Launch new instances

## See Also

- [How-To: Cost Optimization](../../how-to/cost-optimization.md) - Stop/start patterns
- [Tutorial 6: Cost Management](../../tutorials/06-cost-management.md) - Cost strategies
- [AWS EC2 Instance Lifecycle](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-lifecycle.html)
