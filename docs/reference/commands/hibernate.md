# spawn hibernate

Hibernate EC2 instances, preserving RAM contents for fast resume.

## Synopsis

```bash
spawn hibernate [instance-id-or-name]
spawn hibernate --array-id <array-id>
spawn hibernate --array-name <array-name>
```

**Aliases:** `sleep`

## Description

The `hibernate` command hibernates EC2 instances, saving RAM contents to the EBS root volume. Hibernated instances can be quickly resumed with `spawn start`, restoring the exact state including running processes and open files.

**Hibernate vs Stop:**
- **Hibernate:** Saves RAM to disk, instant resume (~30s), processes continue
- **Stop:** Discards RAM, slower resume (~60s), processes restart

**Requirements:**
- Instance must be hibernation-enabled (set at launch)
- Root volume must have space for RAM snapshot
- Specific instance types and AMIs only

**Use cases:**
- Quick pause/resume of workloads
- Preserve large in-memory datasets
- Save application state without checkpointing
- Resume long-running processes

## Options

### Target Selection

**`[instance-id-or-name]`** (positional)
- Hibernate single instance by ID or name
- Example: `i-0abc123def456789` or `my-instance`

**`--array-id <array-id>`**
- Hibernate all instances in a job array by array ID
- Example: `array-20260127-abc123`

**`--array-name <array-name>`**
- Hibernate all instances in a job array by array name
- Example: `ml-training-sweep`

## Arguments

One of the following must be provided:
- Instance ID/name (positional argument)
- `--array-id` flag
- `--array-name` flag

## Requirements

### Hibernation-Enabled at Launch

**Instance must be launched with hibernation enabled:**
```bash
# Enable hibernation at launch
spawn launch \
  --instance-type m7i.xlarge \
  --hibernation-enabled \
  --ttl 4h
```

**Cannot enable after launch:**
- Hibernation must be configured at instance creation
- Existing instances cannot be modified to support hibernation

### Instance Type Support

**Supported families:**
- C3, C4, C5, C6i, C7i
- M3, M4, M5, M6i, M7i
- R3, R4, R5, R6i, R7i
- T2, T3, T3a

**Not supported:**
- GPU instances (G, P, Inf families)
- High memory instances (u-*, x1*, z1d)
- Bare metal instances

**Size limits:**
- Instance RAM must be < 150 GB
- Root volume must have RAM + 1 GB free space

### AMI Requirements

**Hibernation-supported AMIs:**
- Amazon Linux 2023 (default)
- Amazon Linux 2
- Ubuntu 18.04+ (with hibernation agent)
- Windows Server 2016+ (limited support)

**Root volume:**
- Must be EBS-backed (not instance store)
- Must be encrypted
- Type: gp2, gp3, io1, or io2

## Examples

### Hibernate Single Instance

**By instance ID:**
```bash
spawn hibernate i-0abc123def456789
```

**By instance name:**
```bash
spawn hibernate my-ml-training
```

**Output:**
```
Hibernating instance i-0abc123def456789...
Saving 16 GB RAM to EBS volume...
✓ Instance hibernated successfully (30s)
```

### Hibernate and Resume

**Quick pause/resume:**
```bash
# Hibernate for lunch break
spawn hibernate my-dev-instance
# Hibernation saves: 8 GB RAM (took 25s)

# Resume after lunch
spawn start my-dev-instance
# Resume time: 28s
# All processes and state restored
```

### Hibernate Job Array

**By array ID:**
```bash
spawn hibernate --array-id array-20260127-abc123
```

**Output:**
```
Hibernating instances in array array-20260127-abc123...

✓ Hibernated i-0abc123 (worker-0) - 32 GB saved
✓ Hibernated i-0def456 (worker-1) - 32 GB saved
✓ Hibernated i-0ghi789 (worker-2) - 32 GB saved

3 instances hibernated successfully
```

## Use Cases

### 1. Pause ML Training Overnight

**Scenario:** Training in progress, pause overnight to save costs.

```bash
# Evening: Hibernate training
spawn hibernate ml-training-gpu

# RAM saved: 64 GB
# Training state: Preserved
# Model gradients: In memory, not lost

# Morning: Resume training
spawn start ml-training-gpu

# Training continues from exact point
# No checkpoint/restore needed
```

### 2. Preserve Development Environment

**Scenario:** Complex dev setup with many running processes.

```bash
# Hibernate dev instance
spawn hibernate my-dev-instance

# State preserved:
# - Running Docker containers
# - Open database connections
# - tmux sessions
# - Vim buffers
# - SSH agent keys in memory

# Resume instantly
spawn start my-dev-instance
```

### 3. Long-Running Simulation

**Scenario:** Simulation with large in-memory dataset.

```bash
# Simulation running with 128 GB in-memory data
spawn hibernate simulation-instance

# Instead of:
# 1. Serialize 128 GB to disk (slow)
# 2. Stop instance
# 3. Restart and deserialize (slow)

# Hibernation:
# 1. Saves RAM to EBS (~2 min)
# 2. Resume with data still in RAM (~30s)
```

## Hibernation Process

### What Happens During Hibernation

**Phase 1: Save RAM (20-60 seconds)**
```
1. OS receives hibernate signal
2. Suspends all running processes
3. Writes RAM contents to EBS root volume
4. Unmounts filesystems
5. Powers off instance
```

**Phase 2: Hibernated State**
```
- Instance state: stopped
- RAM snapshot: On EBS root volume
- Billing: Same as stopped instance
- Duration: Can stay hibernated indefinitely
```

**Phase 3: Resume (~30 seconds)**
```
1. Instance powers on
2. Reads RAM from EBS volume
3. Restores process state
4. Resumes all processes
5. Instance operational
```

### Time Comparison

| Action | Stop | Hibernate | Terminate + Launch |
|--------|------|-----------|-------------------|
| Pause time | 30-60s | 30-90s | 10-30s |
| Resume time | 60-120s | 30-60s | 60-300s |
| **Total roundtrip** | **90-180s** | **60-150s** | **70-330s** |
| State preserved | Disk only | RAM + Disk | None (from AMI) |

## Hibernation vs Alternatives

### Hibernate

**Pros:**
- Fast resume (30-60s)
- RAM contents preserved
- Processes continue seamlessly
- No application changes needed

**Cons:**
- Requires hibernation-enabled launch
- Root volume size = disk + RAM
- Limited instance type support
- Same stopped instance costs

**Best for:**
- Large in-memory datasets
- Long-running processes
- Quick pause/resume cycles

### Stop

**Pros:**
- Works on all instance types
- No special configuration needed
- Simplest option

**Cons:**
- RAM contents lost
- Slower resume (60-120s)
- Processes must restart

**Best for:**
- Simple pause/resume
- No in-memory state
- Standard instance types

### Checkpoint to S3

**Pros:**
- Works on any instance
- Portable across instances
- Can resume on different instance type

**Cons:**
- Requires application support
- Slower (serialize/deserialize)
- More complex to implement

**Best for:**
- Spot instances (can't hibernate)
- Cross-instance portability
- Long-term state preservation

## Cost Analysis

### Costs While Hibernated

**Same as stopped instance:**
- No compute charges
- EBS volume charges continue
- Elastic IP charges (if attached)

**Additional consideration:**
- Root volume larger (to accommodate RAM)
- Example: 8 GB disk + 32 GB RAM = 40 GB root volume
- Storage cost: 40 GB × $0.10/GB/month = $4.00/month

### Cost Comparison

**Example: m7i.2xlarge (8 vCPU, 32 GB RAM)**

| State | Compute | EBS (40 GB) | Total/Hour | Total/Month |
|-------|---------|-------------|------------|-------------|
| Running | $0.3840 | $0.0055 | $0.3895 | $278.76 |
| Hibernated | $0.0000 | $0.0055 | $0.0055 | $4.00 |
| **Savings** | **100%** | **0%** | **~99%** | **~99%** |

**vs. Terminate/Recreate:**
- AMI snapshot: 40 GB × $0.05/GB/month = $2.00/month
- Cheaper but slower resume (60-300s vs 30s)

## Limitations

### Cannot Hibernate If

**Instance not configured:**
```bash
# ERROR: Instance not hibernation-enabled
spawn hibernate i-0abc123

Error: Instance i-0abc123 does not support hibernation
Hibernation must be enabled at launch with --hibernation-enabled
```

**Solution:** Launch new instance with hibernation enabled.

**Insufficient root volume space:**
```bash
# ERROR: Not enough space for RAM snapshot
spawn hibernate i-0abc123

Error: Root volume has 10 GB free, need 33 GB (32 GB RAM + 1 GB)
```

**Solution:** Launch with larger root volume:
```bash
spawn launch \
  --instance-type m7i.2xlarge \
  --root-volume-size 64 \
  --hibernation-enabled
```

**Spot instances:**
- Spot instances cannot hibernate
- Can only stop or terminate
- Use checkpointing instead

### Hibernation Duration

**No hard limit:**
- Instances can stay hibernated indefinitely
- However:
  - EBS costs accumulate
  - Underlying hardware may change
  - Security patches not applied

**Best practice:**
- Hibernate for hours to days
- For longer pauses, terminate and recreate

## Monitoring Hibernated Instances

### Check Hibernation Status

```bash
# View instance state
spawn status i-0abc123

# Output:
# State: stopped (hibernated)
# RAM saved: 32 GB
# Hibernated at: 2026-01-27 14:30:00 UTC
# Resume with: spawn start i-0abc123
```

### List All Hibernated Instances

```bash
# Find hibernated instances
spawn list --state stopped --json | \
  jq '.[] | select(.hibernated == true) | {instance_id, name, ram_saved_gb}'
```

## Permissions

### Required IAM Permissions

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeInstances",
      "ec2:StopInstances",
      "ec2:StartInstances"
    ],
    "Resource": "*"
  }]
}
```

## Troubleshooting

### Hibernation Fails

**Problem:** "Failed to hibernate instance"

**Common causes:**

1. **Not enough disk space:**
```bash
# Check root volume size
aws ec2 describe-volumes \
  --filters "Name=attachment.instance-id,Values=i-0abc123" \
  --query 'Volumes[0].Size'

# Requires: RAM size + 1 GB
```

2. **Hibernation not enabled:**
```bash
# Check if hibernation enabled
aws ec2 describe-instances \
  --instance-ids i-0abc123 \
  --query 'Reservations[0].Instances[0].HibernationOptions.Configured'

# Must be: true
```

3. **Unsupported instance type:**
```bash
# Check instance type support
# GPU and large memory instances not supported
```

### Slow Hibernation

**Problem:** Hibernation taking minutes.

**Cause:** Large RAM size (saving 128 GB takes time).

**Normal times:**
- 16 GB RAM: 20-30 seconds
- 32 GB RAM: 30-60 seconds
- 64 GB RAM: 60-120 seconds
- 128 GB RAM: 120-180 seconds

**Optimization:**
- Use instances with less RAM if possible
- Consider checkpoint to S3 for very large memory

### Resume Issues

**Problem:** Instance won't resume from hibernation.

**Debugging:**
```bash
# Check instance state
spawn status i-0abc123

# View system logs
aws ec2 get-console-output --instance-id i-0abc123

# Force stop and start (loses hibernated state)
spawn stop i-0abc123
spawn start i-0abc123
```

## Related Commands

- **[spawn start](start.md)** - Resume hibernated instances
- **[spawn stop](stop.md)** - Stop instances (discards RAM)
- **[spawn cancel](cancel.md)** - Terminate instances
- **[spawn launch](launch.md)** - Enable hibernation at launch (`--hibernation-enabled`)
- **[spawn status](status.md)** - Check hibernation status

## See Also

- [How-To: Cost Optimization](../../how-to/cost-optimization.md) - Hibernation cost analysis
- [How-To: Spot Instances](../../how-to/spot-instances.md) - Checkpointing alternatives
- [Tutorial 6: Cost Management](../../tutorials/06-cost-management.md) - Cost patterns
- [AWS Hibernation Documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/Hibernate.html)
