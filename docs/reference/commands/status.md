# spawn status

Check the status of instances or parameter sweeps.

## Synopsis

```bash
spawn status <instance-id-or-name> [flags]
spawn status --sweep-id <sweep-id> [flags]
```

## Description

Display detailed status information for EC2 instances or parameter sweeps, including state, uptime, TTL remaining, resource usage, and sweep progress.

## Usage Modes

### Instance Status
Check status of a single EC2 instance.

```bash
spawn status i-0123456789abcdef0
spawn status my-instance
```

### Sweep Status
Check progress of a parameter sweep.

```bash
spawn status --sweep-id sweep-20260127-abc123
```

## Arguments

### instance-id-or-name
**Type:** String
**Required:** Yes (for instance mode)
**Description:** EC2 instance ID (i-xxxxx) or instance name (Name tag value).

```bash
spawn status i-0123456789abcdef0
spawn status my-training-job
```

## Flags

### Sweep Mode

#### --sweep-id
**Type:** String
**Required:** Yes (for sweep mode)
**Description:** Parameter sweep ID.

```bash
spawn status --sweep-id sweep-20260127-abc123
```

### Output Format

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output in JSON format.

```bash
spawn status i-1234567890 --json
spawn status --sweep-id sweep-123 --json
```

#### --watch, -w
**Type:** Boolean
**Default:** `false`
**Description:** Continuously update status (refresh every 5 seconds).

```bash
spawn status --sweep-id sweep-123 --watch
# Press Ctrl+C to exit
```

#### --refresh
**Type:** Duration
**Default:** `5s`
**Description:** Refresh interval for `--watch` mode.

```bash
spawn status --sweep-id sweep-123 --watch --refresh 10s
```

## Output

### Instance Status (Standard)

```
Instance: i-0123456789abcdef0
Name: my-training-job
Region: us-east-1
Availability Zone: us-east-1a
State: running

Instance Type: g5.xlarge
Architecture: x86_64
Platform: Amazon Linux 2023

Network:
  Public IP: 54.123.45.67
  Private IP: 10.0.1.100
  VPC: vpc-0123456789abcdef0
  Subnet: subnet-0123456789abcdef0
  DNS: my-training-job.c0zxr0ao.spore.host

Lifecycle:
  Launch Time: 2026-01-27 10:00:00 PST
  Uptime: 2h 15m
  TTL: 8h (5h 45m remaining)
  Idle Timeout: 1h (not idle)
  On Complete: terminate

Storage:
  Root Volume: 100 GB gp3 (encrypted)
  Hibernation: Enabled

IAM:
  Role: ml-training-role-a1b2c3d4
  Policies: s3:ReadOnly, logs:WriteOnly

Tags:
  Name: my-training-job
  spawn:managed: true
  spawn:ttl: 8h
  env: prod
  project: ml
```

### Instance Status (JSON)

```json
{
  "instance_id": "i-0123456789abcdef0",
  "name": "my-training-job",
  "region": "us-east-1",
  "availability_zone": "us-east-1a",
  "state": "running",
  "instance_type": "g5.xlarge",
  "architecture": "x86_64",
  "platform": "Amazon Linux 2023",
  "network": {
    "public_ip": "54.123.45.67",
    "private_ip": "10.0.1.100",
    "vpc_id": "vpc-0123456789abcdef0",
    "subnet_id": "subnet-0123456789abcdef0",
    "dns_name": "my-training-job.c0zxr0ao.spore.host"
  },
  "lifecycle": {
    "launch_time": "2026-01-27T10:00:00-08:00",
    "uptime": "2h15m",
    "ttl": "8h",
    "ttl_remaining": "5h45m",
    "ttl_expires_at": "2026-01-27T18:00:00-08:00",
    "idle_timeout": "1h",
    "is_idle": false,
    "on_complete": "terminate"
  },
  "storage": {
    "root_volume_size": 100,
    "root_volume_type": "gp3",
    "encrypted": true,
    "hibernation_enabled": true
  },
  "iam": {
    "role_name": "ml-training-role-a1b2c3d4",
    "policies": ["s3:ReadOnly", "logs:WriteOnly"]
  },
  "tags": {
    "Name": "my-training-job",
    "spawn:managed": "true",
    "spawn:ttl": "8h",
    "env": "prod",
    "project": "ml"
  }
}
```

### Sweep Status (Standard)

```
Sweep: sweep-20260127-abc123
Status: running
Started: 2026-01-27 14:00:00 PST
Elapsed: 1h 23m

Progress:
  Total Parameters: 50
  Launched: 28 (56.0%)
  Running: 5
  Completed: 23
  Failed: 0

Concurrency:
  Max Concurrent: 10
  Currently Running: 5
  Launch Delay: 5s

Next Launch:
  Parameter: run-029
  Scheduled: 2026-01-27 15:23:10 PST (in 5s)

Estimated Completion:
  At current rate: 2026-01-27 16:30:00 PST (in 1h 7m)
  With max concurrency: 2026-01-27 15:45:00 PST (in 22m)

Recent Launches:
  run-028  i-0123456789abc  running   us-east-1a  14:23:05
  run-027  i-0987654321fed  running   us-east-1b  14:23:00
  run-026  i-abcdef012345f  completed us-east-1a  14:22:55
  run-025  i-543210fedcba9  completed us-east-1c  14:22:50
  run-024  i-135792468ace0  running   us-east-1a  14:22:45
```

### Sweep Status (JSON)

```json
{
  "sweep_id": "sweep-20260127-abc123",
  "status": "running",
  "started_at": "2026-01-27T14:00:00-08:00",
  "elapsed": "1h23m",
  "progress": {
    "total": 50,
    "launched": 28,
    "launched_percent": 56.0,
    "running": 5,
    "completed": 23,
    "failed": 0
  },
  "concurrency": {
    "max_concurrent": 10,
    "current_running": 5,
    "launch_delay": "5s"
  },
  "next_launch": {
    "parameter_name": "run-029",
    "parameter_index": 28,
    "scheduled_at": "2026-01-27T15:23:10-08:00",
    "in_seconds": 5
  },
  "estimated_completion": {
    "current_rate": "2026-01-27T16:30:00-08:00",
    "max_concurrency": "2026-01-27T15:45:00-08:00"
  },
  "recent_launches": [
    {
      "parameter_name": "run-028",
      "instance_id": "i-0123456789abc",
      "state": "running",
      "availability_zone": "us-east-1a",
      "launched_at": "2026-01-27T14:23:05-08:00"
    }
  ]
}
```

## Examples

### Check Instance Status
```bash
# By instance ID
spawn status i-0123456789abcdef0

# By instance name
spawn status my-training-job

# JSON output
spawn status i-1234567890 --json
```

### Check Sweep Status
```bash
# Basic sweep status
spawn status --sweep-id sweep-20260127-abc123

# Watch sweep progress (live updates)
spawn status --sweep-id sweep-20260127-abc123 --watch

# JSON for automation
spawn status --sweep-id sweep-20260127-abc123 --json
```

### Monitor Sweep in Script
```bash
#!/bin/bash
SWEEP_ID="sweep-20260127-abc123"

while true; do
    STATUS=$(spawn status --sweep-id "$SWEEP_ID" --json)
    STATE=$(echo "$STATUS" | jq -r '.status')

    if [[ "$STATE" == "completed" ]]; then
        echo "Sweep complete!"
        FAILED=$(echo "$STATUS" | jq -r '.progress.failed')
        echo "Failed launches: $FAILED"
        break
    elif [[ "$STATE" == "failed" ]]; then
        echo "Sweep failed!"
        exit 1
    fi

    LAUNCHED=$(echo "$STATUS" | jq -r '.progress.launched')
    TOTAL=$(echo "$STATUS" | jq -r '.progress.total')
    echo "Progress: $LAUNCHED / $TOTAL"

    sleep 30
done
```

### Check TTL Remaining
```bash
# Get TTL remaining for instance
TTL=$(spawn status i-1234567890 --json | jq -r '.lifecycle.ttl_remaining')
echo "Time remaining: $TTL"

# Extend if less than 1 hour
if [[ "$TTL" =~ ^[0-9]+m$ ]]; then
    MINUTES=${TTL%m}
    if [ "$MINUTES" -lt 60 ]; then
        echo "TTL low, extending"
        spawn extend i-1234567890 2h
    fi
fi
```

### Check if Instance is Idle
```bash
IS_IDLE=$(spawn status i-1234567890 --json | jq -r '.lifecycle.is_idle')
if [ "$IS_IDLE" == "true" ]; then
    echo "Instance is idle, will terminate soon"
fi
```

### Monitor All Running Instances
```bash
# Check status of all running instances
for instance in $(spawn list --state running --quiet); do
    echo "=== $instance ==="
    spawn status "$instance"
    echo ""
done
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Status retrieved successfully |
| 1 | Status check failed (AWS API error, network error) |
| 2 | Invalid arguments (missing instance ID or sweep ID) |
| 3 | Resource not found (instance or sweep doesn't exist) |

## State Values

### Instance States
- `pending` - Instance is launching
- `running` - Instance is running
- `stopping` - Instance is stopping
- `stopped` - Instance is stopped
- `shutting-down` - Instance is terminating
- `terminated` - Instance is terminated

### Sweep States
- `pending` - Sweep is queued, not started
- `running` - Sweep is actively launching instances
- `paused` - Sweep is paused (manual intervention)
- `completed` - All instances launched successfully
- `failed` - Sweep failed (critical error)
- `cancelled` - Sweep was cancelled by user

## Performance

- **Instance Status:** ~200-500ms (EC2 DescribeInstances API call)
- **Sweep Status:** ~100-200ms (DynamoDB query)
- **Watch Mode:** Cached for refresh interval (default 5s)

## Troubleshooting

### "Instance not found"
```bash
# Check if instance exists
spawn list | grep i-1234567890

# Check region
spawn status i-1234567890 --region us-west-2
```

### "Sweep not found"
```bash
# List all sweeps
spawn list-sweeps

# Check sweep ID format
spawn status --sweep-id sweep-20260127-abc123  # Correct format
```

### Incomplete Information
```bash
# Some fields may be empty if:
# - Instance just launched (pending state)
# - Network not configured yet
# - spored agent not installed yet

# Wait and retry
sleep 30
spawn status i-1234567890
```

## See Also
- [spawn launch](launch.md) - Launch instances
- [spawn list](list.md) - List instances
- [spawn extend](extend.md) - Extend instance TTL
- [spawn cancel](cancel.md) - Cancel sweeps
- [spawn list-sweeps](list-sweeps.md) - List all sweeps
