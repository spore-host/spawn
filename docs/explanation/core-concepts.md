# Core Concepts

Deep dive into spawn's fundamental concepts: TTL, idle detection, and spot interruption handling.

## Time-To-Live (TTL)

### What is TTL?

Time-To-Live (TTL) is the maximum lifetime of a spawn instance. When TTL expires, the instance automatically terminates.

**Purpose:**
- Prevent runaway costs from forgotten instances
- Enforce ephemeral instance pattern
- Automatic cleanup without manual intervention

### How TTL Works

**Set at launch:**
```bash
spawn launch --instance-type c7i.xlarge --ttl 2h
```

**TTL countdown:**
```
Launch time:  2026-01-27 10:00:00 UTC
TTL:          2h (7200 seconds)
Expiration:   2026-01-27 12:00:00 UTC

Remaining time calculation:
  Current time - Launch time = Elapsed
  TTL - Elapsed = Remaining
```

**spored monitoring:**
1. Reads TTL from config every 30 seconds
2. Calculates remaining time
3. Warns users when < 10 minutes remaining
4. Terminates instance at TTL=0

### TTL Warning System

**Warning timeline:**
```
TTL remaining: 10 minutes
  └─> wall message to all users: "Instance will terminate in 10 minutes"

TTL remaining: 5 minutes
  └─> wall message: "Instance will terminate in 5 minutes"

TTL remaining: 1 minute
  └─> wall message: "Instance will terminate in 1 minute"

TTL remaining: 0
  └─> Cleanup and terminate
```

**wall message example:**
```
Broadcast message from spored@ip-10-0-1-42

Instance will terminate in 10 minutes (TTL expiring)
Extend with: spawn extend i-0abc123def456789 --ttl 2h
```

### Extending TTL

**Before expiration:**
```bash
# Add 2 more hours
spawn extend i-0abc123 --ttl 2h

# Or disable TTL entirely
spawn config i-0abc123 set ttl.enabled false
```

**After expiration:**
- Cannot extend (instance already terminated)
- Must launch new instance
- Consider saving checkpoints before TTL expires

### TTL vs Idle Timeout

**TTL (absolute time):**
- Counts from launch time
- Independent of activity
- Instance terminates even if active

**Idle timeout (inactivity time):**
- Counts from last activity
- Resets when activity detected
- Only terminates if idle

**Combined behavior:**
```bash
spawn launch --ttl 8h --idle-timeout 1h

# Instance will terminate when:
# - 8 hours pass (TTL), OR
# - Idle for 1 hour (idle timeout)
# Whichever comes first
```

### TTL Best Practices

**Set appropriate durations:**
```bash
# Quick experiments
spawn launch --ttl 30m

# Standard workloads
spawn launch --ttl 2h

# Long-running jobs
spawn launch --ttl 8h

# Very long jobs
spawn launch --ttl 24h
```

**Don't rely on extensions:**
- Extensions are manual operations
- Easy to forget
- Better: Set sufficient TTL upfront

**Use checkpointing for long jobs:**
```bash
# Save state periodically
while not_done():
    compute()
    if iteration % 10 == 0:
        save_checkpoint()
```

### Disabling TTL

**When to disable:**
- Interactive development (unpredictable duration)
- Waiting for external events
- Testing/debugging (don't want surprise termination)

**How to disable:**
```bash
# At launch
spawn launch --ttl 0  # 0 = infinite

# On running instance
spawn config i-0abc123 set ttl.enabled false
```

**⚠️ Warning:** Disabled TTL means no automatic cleanup. You MUST manually terminate.

## Idle Detection

### What is Idle Detection?

Idle detection automatically terminates instances that aren't doing work, saving costs.

**Enabled by default:** No (opt-in feature)

**Metrics monitored:**
- CPU utilization
- Network I/O
- Disk I/O (optional)

### How Idle Detection Works

**Enable at launch:**
```bash
spawn launch --idle-timeout 30m
```

**Detection algorithm:**
```
Every 30 seconds:
  1. Sample CPU utilization (average over last minute)
  2. Sample network bytes in/out
  3. If CPU < threshold AND network < threshold:
     Increment idle counter
  4. Else:
     Reset idle counter to 0

If idle counter * sample_interval >= idle_timeout:
  Terminate instance
```

**Example:**
```bash
spawn launch --idle-timeout 30m

# Instance activity:
10:00 - Launch (CPU: 80%)
10:05 - Job completes (CPU: 2%)
10:05:30 - Idle check: CPU low, counter = 1
10:06:00 - Idle check: CPU low, counter = 2
...
10:35:00 - Idle check: counter = 60 (30 minutes)
10:35:00 - Terminate instance
```

### CPU Threshold

**Default:** 5% CPU utilization

**Customize:**
```bash
spawn config i-0abc123 set idle.cpu_threshold 10.0
```

**Considerations:**
- Too low (1%): May false positive (system processes)
- Too high (20%): May miss truly idle instances
- Recommended: 5-10% for most workloads

### Network Threshold

**Default:** 1 KB/s average

**Why network matters:**
- Instance may be I/O bound (low CPU, high network)
- Downloading data uses network, not CPU
- Uploading results uses network, not CPU

**Example: False idle without network check:**
```
Task: Download 100 GB dataset
CPU usage: 3% (I/O wait)
Network: 100 MB/s download
Without network check: Considered idle (WRONG)
With network check: Considered active (CORRECT)
```

### Idle Detection Pitfalls

**Problem 1: Periodic jobs**
```bash
# Cron job runs every hour for 5 minutes
# Idle for 55 minutes, active for 5 minutes
# With 30m idle timeout: Terminates during idle period
```

**Solution:** Disable idle detection or use longer timeout
```bash
spawn launch --idle-timeout 0  # Disabled
# OR
spawn launch --idle-timeout 60m  # Longer than idle periods
```

**Problem 2: Interactive development**
```bash
# Developer coding, thinking, reading docs
# SSH connected but low CPU
# May be terminated while developer is working
```

**Solution:** Disable idle detection for interactive instances
```bash
spawn launch --instance-type t3.medium  # No idle timeout
```

**Problem 3: Waiting for external events**
```bash
# Instance polling SQS queue (empty)
# Low CPU while waiting
# Terminates before messages arrive
```

**Solution:** Keep-alive heartbeat
```python
import time

while True:
    messages = poll_queue()
    if messages:
        process(messages)
    else:
        # Keep-alive: Do some CPU work
        _ = sum(range(10000))
        time.sleep(5)
```

### Combining Idle and TTL

**Pattern 1: Quick timeout with fallback**
```bash
spawn launch --ttl 2h --idle-timeout 15m

# Terminates after 15 minutes if idle
# Or 2 hours regardless (safety net)
```

**Pattern 2: Long TTL with aggressive idle**
```bash
spawn launch --ttl 8h --idle-timeout 30m

# For batch jobs that may finish early
# Don't wait full 8 hours if job completes
```

## Spot Interruption Handling

### What are Spot Interruptions?

AWS can reclaim spot instances with 2-minute warning when capacity is needed for on-demand.

**Interruption notice:**
- 2 minutes advance warning
- Provided via instance metadata endpoint
- Instance terminates automatically (not by spored)

### How spored Monitors Spot Interruptions

**Polling loop:**
```
Every 5 seconds:
  1. Check if instance is spot (via metadata)
  2. If spot, poll: http://169.254.169.254/latest/meta-data/spot/instance-action
  3. If endpoint returns data:
     Parse interruption time
     Run cleanup actions
```

**Metadata endpoint behavior:**
- **No interruption:** HTTP 404 (not found)
- **Interruption pending:** HTTP 200 with JSON body

**Example interruption notice:**
```json
{
  "action": "terminate",
  "time": "2026-01-27T12:35:00Z"
}
```

### Cleanup Actions on Interruption

**spored's interruption handler:**
```
1. Log alert: "Spot interruption in 2 minutes"

2. Send wall message to users:
   "This spot instance will be terminated by AWS in 2 minutes"

3. Deregister DNS (via Lambda)

4. Send notifications (if configured):
   - Slack alert
   - Email notification
   - SNS publish

5. Run user-defined pre-termination hook (if configured):
   - Save checkpoint
   - Upload results
   - Custom cleanup

6. Write marker file: /tmp/spawn-spot-interruption.json
   {
     "interrupted_at": "2026-01-27T12:33:00Z",
     "termination_time": "2026-01-27T12:35:00Z"
   }

7. Wait for AWS to terminate (don't call TerminateInstances)
```

### User Workload Handling

**Option 1: Trap SIGTERM**
```bash
#!/bin/bash

cleanup() {
    echo "Interrupted, saving checkpoint..."
    python save_checkpoint.py
    aws s3 cp checkpoint.pkl s3://bucket/checkpoint-latest.pkl
    exit 0
}

trap cleanup SIGTERM

# Main workload
python train.py
```

**Option 2: Poll marker file**
```python
import os
import time

while training:
    # Check for interruption
    if os.path.exists('/tmp/spawn-spot-interruption.json'):
        print("Spot interruption detected, saving checkpoint...")
        save_checkpoint('s3://bucket/checkpoint.pkl')
        break

    # Train one step
    train_step()
```

**Option 3: Check metadata directly**
```python
import requests

def is_interrupted():
    try:
        r = requests.get(
            'http://169.254.169.254/latest/meta-data/spot/instance-action',
            timeout=1
        )
        return r.status_code == 200
    except:
        return False

while training:
    if is_interrupted():
        save_checkpoint()
        break
    train_step()
```

### Spot Interruption Patterns

**Pattern 1: Checkpoint every N steps**
```python
for step in range(max_steps):
    train_step()

    if step % 100 == 0:
        save_checkpoint(f'checkpoint-{step}.pkl')

    if is_interrupted():
        print(f"Interrupted at step {step}")
        break
```

**Pattern 2: Checkpoint on SIGTERM**
```bash
#!/bin/bash

cleanup() {
    echo "Saving final checkpoint..."
    python save_checkpoint.py
}

trap cleanup SIGTERM INT

python train.py

# If we get here, training completed normally
echo "Training completed"
```

**Pattern 3: Resume from checkpoint**
```python
# Load checkpoint if exists
if checkpoint_exists():
    state = load_checkpoint()
    start_step = state['step']
else:
    start_step = 0

for step in range(start_step, max_steps):
    train_step()

    if step % 100 == 0:
        save_checkpoint({'step': step, ...})
```

### Spot Interruption Frequency

**Typical rates:**
- **c/m family:** 5-10% monthly interruption rate
- **g family (GPU):** 10-20% monthly interruption rate
- **p family (ML):** 15-30% monthly interruption rate

**Higher interruption zones:**
- us-east-1 (high demand)
- During business hours (9-5 EST)
- GPU instances (limited capacity)

**Lower interruption zones:**
- us-west-2, eu-west-1
- Off-peak hours
- CPU-only instances

### When NOT to Use Spot

**Avoid spot for:**
- Jobs without checkpointing
- Latency-sensitive workloads
- Short jobs (< 10 minutes) - not worth interruption risk
- Critical production workloads

**Use on-demand instead:**
```bash
spawn launch --instance-type c7i.xlarge  # No --spot flag
```

## Interaction Between Concepts

### Priority Order

When multiple termination conditions exist:

**1. Spot interruption (highest priority)**
- Cannot be prevented
- 2-minute warning
- AWS terminates, not spored

**2. TTL expiration**
- spored terminates
- Predictable timing
- Can be extended

**3. Idle timeout**
- spored terminates
- Based on activity detection
- Can be disabled

### Example Scenarios

**Scenario 1: Spot + TTL + Idle**
```bash
spawn launch --spot --ttl 4h --idle-timeout 30m

# Instance will terminate when:
# - Spot interruption (any time), OR
# - 4 hours pass (TTL), OR
# - Idle for 30 minutes
# Whichever happens FIRST
```

**Scenario 2: Long-running spot job**
```bash
spawn launch --spot --ttl 24h

# Likely to be interrupted before 24h
# Must implement checkpointing
# Resume on new instance
```

**Scenario 3: Interactive dev**
```bash
spawn launch --ttl 8h  # No --spot, no --idle-timeout

# Only terminates after 8 hours
# Safe for interactive work
# Manual cleanup if finished early
```

## Related Documentation

- [Architecture Overview](architecture.md) - System design
- [How-To: Spot Instances](../how-to/spot-instances.md) - Spot patterns
- [How-To: Cost Optimization](../how-to/cost-optimization.md) - TTL and idle strategies
- [spawn extend command](../reference/commands/extend.md) - Extend TTL
- [spawn config command](../reference/commands/config.md) - Configure TTL/idle
