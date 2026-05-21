# Parameter Sweeps Guide

Complete guide to running parameter sweeps with spawn CLI.

---

## Table of Contents

- [Quick Start](#quick-start)
- [What Are Parameter Sweeps?](#what-are-parameter-sweeps)
- [Parameter File Format](#parameter-file-format)
- [CLI Commands](#cli-commands)
- [Detached Mode](#detached-mode)
- [Advanced Features](#advanced-features)
- [Use Cases & Examples](#use-cases--examples)
- [Troubleshooting](#troubleshooting)
- [Best Practices](#best-practices)

---

## Quick Start

Launch 3 instances with different parameters in under 5 minutes:

```bash
# 1. Create parameter file
cat > sweep.json << 'EOF'
{
  "defaults": {
    "instance_type": "t3.micro",
    "region": "us-east-1",
    "ttl": "30m"
  },
  "params": [
    {"name": "alpha-0.1", "learning_rate": 0.1},
    {"name": "alpha-0.5", "learning_rate": 0.5},
    {"name": "alpha-0.9", "learning_rate": 0.9}
  ]
}
EOF

# 2. Launch sweep (CLI-orchestrated)
spawn launch --param-file sweep.json --max-concurrent 2

# OR launch in detached mode (Lambda-orchestrated)
spawn launch --param-file sweep.json --max-concurrent 2 --detach
```

That's it! Spawn will launch instances with your parameters.

---

## What Are Parameter Sweeps?

**Parameter sweeps** let you launch multiple EC2 instances with different configurations in a coordinated way. Each instance gets its own set of parameters while sharing common defaults.

### Common Use Cases

- **Hyperparameter tuning** - Test different learning rates, batch sizes, etc.
- **A/B testing** - Compare different configurations
- **Multi-configuration testing** - Test across regions, instance types, or settings
- **Batch processing** - Process different datasets or inputs in parallel

### Two Orchestration Modes

**CLI-Orchestrated (default):**
- Runs from your terminal
- Requires keeping terminal open
- Local state in `~/.spawn/sweeps/`
- Good for: Quick tests, local development

**Lambda-Orchestrated (--detach):**
- Runs in AWS Lambda
- Survives laptop disconnection
- Remote state in DynamoDB
- Good for: Long-running sweeps, production use
- **Recommended for sweeps >30 minutes**

See [Detached Mode](#detached-mode) for details.

---

## Parameter File Format

Parameter files are JSON (or YAML) with two sections: `defaults` and `params`.

### Basic Structure

```json
{
  "defaults": {
    "instance_type": "t3.micro",
    "region": "us-east-1",
    "ami": "ami-07ff62358b87c7116",
    "key_name": "spawn-key-myname",
    "iam_role": "spawnd-role",
    "ttl": "1h",
    "idle_timeout": "15m"
  },
  "params": [
    {"name": "test-1", "alpha": 0.1, "beta": 0.01},
    {"name": "test-2", "alpha": 0.5, "beta": 0.05},
    {"name": "test-3", "alpha": 0.9, "beta": 0.09}
  ]
}
```

### Defaults Section

**Common fields** shared by all instances unless overridden:

| Field | Description | Example |
|-------|-------------|---------|
| `instance_type` | EC2 instance type | `"t3.micro"`, `"g5.xlarge"` |
| `region` | AWS region | `"us-east-1"`, `"us-west-2"` |
| `ami` | AMI ID (auto-detected if omitted) | `"ami-07ff62358b87c7116"` |
| `key_name` | SSH key pair name | `"spawn-key-myname"` |
| `iam_role` | IAM instance profile | `"spawnd-role"` |
| `ttl` | Time-to-live (auto-terminate) | `"30m"`, `"2h"`, `"4h30m"` |
| `idle_timeout` | Idle timeout (auto-terminate) | `"5m"`, `"15m"`, `"1h"` |
| `spot` | Use Spot instances | `true` or `false` |
| `user_data` | User data script | `"#!/bin/bash\necho hello"` |

Any field accepted by `spawn launch` can go in defaults.

### Params Section

**Per-instance parameters** - array of objects, one per instance:

```json
{
  "params": [
    {
      "name": "config-1",           // Instance name (optional)
      "learning_rate": 0.001,       // Custom parameter
      "batch_size": 32,             // Custom parameter
      "instance_type": "g5.xlarge"  // Override default
    },
    {
      "name": "config-2",
      "learning_rate": 0.01,
      "batch_size": 64
    }
  ]
}
```

**Rules:**
- Each object becomes one instance
- Can override any default field
- Can add custom fields (accessible in instance)
- Order preserved (index 0, 1, 2, ...)

### How Parameters Are Merged

```json
{
  "defaults": {"instance_type": "t3.micro", "region": "us-east-1"},
  "params": [
    {"name": "test-1", "alpha": 0.5},
    {"name": "test-2", "alpha": 0.9, "region": "us-west-2"}
  ]
}
```

**Instance 0 gets:**
- `instance_type`: `"t3.micro"` (from defaults)
- `region`: `"us-east-1"` (from defaults)
- `name`: `"test-1"` (from params)
- `alpha`: `0.5` (from params)

**Instance 1 gets:**
- `instance_type`: `"t3.micro"` (from defaults)
- `region`: `"us-west-2"` (override)
- `name`: `"test-2"` (from params)
- `alpha`: `0.9` (from params)

---

## CLI Commands

### Launch Sweep

```bash
# CLI-orchestrated (default)
spawn launch --param-file sweep.json --max-concurrent 5

# Lambda-orchestrated (detached mode)
spawn launch --param-file sweep.json --max-concurrent 5 --detach

# With launch delay between instances
spawn launch --param-file sweep.json --max-concurrent 5 --launch-delay 10s

# With custom sweep name
spawn launch --param-file sweep.json --max-concurrent 5 --sweep-name ml-experiment
```

**Flags:**
- `--param-file <path>` - Path to parameter file (required)
- `--max-concurrent <n>` - Max instances running simultaneously (required for rolling queue)
- `--detach` - Use Lambda orchestration (survives disconnection)
- `--launch-delay <duration>` - Delay between launches (default: 0s)
- `--sweep-name <name>` - Human-readable sweep identifier (auto-generated if omitted)

**Without --max-concurrent:**
All instances launch immediately (parallel launch, no queuing).

**With --max-concurrent:**
Rolling queue - launches N instances, waits for termination, launches next batch.

### Check Status (CLI-orchestrated)

```bash
# List all sweeps
spawn list

# Show sweep details
spawn list --sweep-name ml-experiment
```

### Check Status (Detached)

```bash
# Show detached sweep status
spawn status --sweep-id sweep-20260116-abc123

# Auto-refresh every 5 seconds
watch -n 5 spawn status --sweep-id sweep-20260116-abc123
```

### Resume Sweep

```bash
# Resume CLI-orchestrated sweep
spawn resume --sweep-id hyperparam-20260115-123456

# Resume in detached mode
spawn resume --sweep-id hyperparam-20260115-123456 --detach

# Resume with different max-concurrent
spawn resume --sweep-id <id> --max-concurrent 10
```

### Cancel Sweep

```bash
# Cancel detached sweep (terminates all instances)
spawn cancel --sweep-id sweep-20260116-abc123
```

---

## Detached Mode

Run sweeps in Lambda to survive laptop disconnection.

### Why Use Detached Mode?

✅ **Survive disconnection** - Close your laptop, switch networks, no problem
✅ **Monitor remotely** - Check status from any machine with AWS credentials
✅ **Long-running sweeps** - Multi-hour sweeps without terminal babysitting
✅ **Resume from anywhere** - Resume from checkpoint on any machine
✅ **Low cost** - ~$0.005 per sweep (half a cent)

### How It Works

1. **Upload** - Parameters uploaded to S3
2. **Queue** - Sweep record created in DynamoDB
3. **Invoke** - Lambda function invoked with sweep ID
4. **Exit** - CLI exits immediately
5. **Orchestrate** - Lambda polls EC2, launches instances with rolling queue
6. **Complete** - Lambda marks sweep as COMPLETED when done

```
CLI                     Lambda                      EC2 Instances
───                     ──────                      ─────────────
Upload params to S3 -->
Create DynamoDB rec -->
Invoke Lambda -------->
Exit immediately        Poll every 10s -----------> Launch instances
                        Check active count
                        Launch next batch --------> Launch more
                        Re-invoke self (13min)
                        Mark COMPLETED
```

### Launch Detached Sweep

```bash
spawn launch --param-file sweep.json --max-concurrent 5 --detach
```

**Output:**
```
✅ Parameter sweep queued successfully!

Sweep ID:          sweep-20260116-abc123
Sweep Name:        sweep
Total Parameters:  50
Max Concurrent:    5
Region:            us-east-1
Orchestration:     Lambda (spore-host-infra account)

The sweep is now running in Lambda. You can disconnect safely.

To check status:
  spawn status --sweep-id sweep-20260116-abc123

To resume if needed:
  spawn resume --sweep-id sweep-20260116-abc123 --detach
```

### Monitor Progress

```bash
spawn status --sweep-id sweep-20260116-abc123
```

**Output:**
```
╔═══════════════════════════════════════════════════════════════╗
║  Parameter Sweep Status                                      ║
╚═══════════════════════════════════════════════════════════════╝

Sweep ID:          sweep-20260116-abc123
Sweep Name:        ml-training
Status:            🚀 RUNNING
Region:            us-east-1

Created:           2026-01-16 10:04:43 PST
Last Updated:      2026-01-16 10:24:15 PST

Progress:
  Total Parameters:  50
  Launched:          23 (46.0%)
  Next to Launch:    23
  Failed:            1
  Est. Completion:   11:45 AM PST (in 1h 21m)

Configuration:
  Max Concurrent:    5
  Launch Delay:      0s

Instances:
  Active:            5
  Completed:         17
  Failed:            1

Recent Instances (showing last 10):
Index Instance ID          State           Launched At
----- -------------------- --------------- --------------------
13    i-abc123def456       🔄 running      2026-01-16 10:20:12
14    i-def789abc012       🔄 running      2026-01-16 10:21:15
...

Failed Launches:
  [8] run instances failed: operation error EC2: RunInstances,
      api error InvalidParameterValue: Invalid instance type
```

### Resume Detached Sweep

If Lambda stops or you need to resume:

```bash
spawn resume --sweep-id sweep-20260116-abc123 --detach
```

Lambda re-invokes and continues from checkpoint (NextToLaunch index).

### Cancel Detached Sweep

```bash
spawn cancel --sweep-id sweep-20260116-abc123
```

Terminates all running/pending instances and marks sweep as CANCELLED.

### Cost & Performance

**Cost per sweep (100 instances, max_concurrent=5, 40min):**
- Lambda: $0.004
- DynamoDB: $0.0003
- S3: $0.000005
- **Total: ~$0.005**

**Performance:**
- Polling interval: 10 seconds
- Launch time: Sub-second per instance
- Max parameter sets: 1000+ (S3 supports unlimited size)
- Max sweep duration: Unlimited (Lambda self-reinvokes)

See [DETACHED_MODE.md](DETACHED_MODE.md) for architecture details.

---

## Advanced Features

### Max Concurrent Limiting

Control how many instances run simultaneously:

```bash
# Launch 50 instances, max 5 concurrent
spawn launch --param-file sweep.json --max-concurrent 5 --detach
```

**Rolling queue behavior:**
1. Launch first 5 instances
2. Poll every 10s
3. When instance terminates, launch next
4. Repeat until all launched

### Launch Delay

Add delay between launches (rate limiting):

```bash
spawn launch --param-file sweep.json --max-concurrent 5 --launch-delay 5s
```

Useful for:
- Avoiding API rate limits
- Staggering resource usage
- Testing sequential vs parallel behavior

### Sweep Naming

Give sweeps human-readable names:

```bash
spawn launch --param-file sweep.json --sweep-name ml-training-batch-3
```

Auto-generated if omitted: `sweep-20260116-abc123`

### TTL and Idle Timeout

Set per-instance auto-termination:

```json
{
  "defaults": {
    "ttl": "2h",           // Terminate after 2 hours
    "idle_timeout": "15m"  // Terminate if idle for 15 minutes
  },
  "params": [...]
}
```

### Custom Parameters

Add any JSON fields - accessible in instance:

```json
{
  "params": [
    {
      "name": "experiment-1",
      "learning_rate": 0.001,
      "batch_size": 32,
      "optimizer": "adam",
      "dataset": "imagenet"
    }
  ]
}
```

Access via environment variables (future) or parse parameter file.

### Parameter Validation

Pre-launch validation (best-effort):

```bash
spawn launch --param-file sweep.json --detach
# Validates instance types exist in target regions
# Warns if validation fails, continues gracefully
```

---

## Use Cases & Examples

### Hyperparameter Tuning

Test different learning rates and batch sizes:

```json
{
  "defaults": {
    "instance_type": "g5.xlarge",
    "region": "us-east-1",
    "ttl": "4h",
    "iam_role": "ml-training-role"
  },
  "params": [
    {"name": "lr-0.001-bs-32", "lr": 0.001, "bs": 32},
    {"name": "lr-0.001-bs-64", "lr": 0.001, "bs": 64},
    {"name": "lr-0.001-bs-128", "lr": 0.001, "bs": 128},
    {"name": "lr-0.01-bs-32", "lr": 0.01, "bs": 32},
    {"name": "lr-0.01-bs-64", "lr": 0.01, "bs": 64},
    {"name": "lr-0.01-bs-128", "lr": 0.01, "bs": 128}
  ]
}
```

```bash
spawn launch --param-file hyperparam.json --max-concurrent 3 --detach
```

### A/B Testing

Compare two configurations:

```json
{
  "defaults": {
    "instance_type": "t3.medium",
    "region": "us-east-1",
    "ttl": "2h"
  },
  "params": [
    {"name": "baseline", "config": "default", "cache_enabled": false},
    {"name": "optimized", "config": "tuned", "cache_enabled": true}
  ]
}
```

### Multi-Region Testing

Test across regions:

```json
{
  "defaults": {
    "instance_type": "t3.micro",
    "ttl": "1h"
  },
  "params": [
    {"name": "us-east", "region": "us-east-1"},
    {"name": "us-west", "region": "us-west-2"},
    {"name": "eu-west", "region": "eu-west-1"},
    {"name": "ap-northeast", "region": "ap-northeast-1"}
  ]
}
```

### Batch Processing

Process different datasets:

```json
{
  "defaults": {
    "instance_type": "c7i.xlarge",
    "region": "us-east-1",
    "ttl": "30m"
  },
  "params": [
    {"name": "dataset-2024-01", "input_path": "s3://data/2024-01/"},
    {"name": "dataset-2024-02", "input_path": "s3://data/2024-02/"},
    {"name": "dataset-2024-03", "input_path": "s3://data/2024-03/"}
  ]
}
```

### Grid Search

Generate grid programmatically:

```python
import json

learning_rates = [0.001, 0.01, 0.1]
batch_sizes = [16, 32, 64]

params = []
for lr in learning_rates:
    for bs in batch_sizes:
        params.append({
            "name": f"lr-{lr}-bs-{bs}",
            "learning_rate": lr,
            "batch_size": bs
        })

sweep = {
    "defaults": {
        "instance_type": "g5.xlarge",
        "region": "us-east-1",
        "ttl": "4h"
    },
    "params": params
}

with open("grid-search.json", "w") as f:
    json.dump(sweep, f, indent=2)
```

```bash
spawn launch --param-file grid-search.json --max-concurrent 3 --detach
```

---

## Troubleshooting

### Failed Launches

**Symptom:** Status shows "Failed: N"

**Check:**
```bash
spawn status --sweep-id <id>
# Look at "Failed Launches" section
```

**Common causes:**
- Invalid instance type for region
- AMI not available in region
- IAM role doesn't exist
- Security group/subnet issues
- Capacity not available (Spot)

**Solution:**
- Fix parameter file
- Use `spawn resume --sweep-id <id>` to retry

### Sweep Not Progressing

**Symptom:** Launched count not increasing

**Check:**
```bash
# For detached sweeps:
AWS_PROFILE=spore-host-infra aws logs tail /aws/lambda/spawn-sweep-orchestrator --follow

# Check Lambda is running
AWS_PROFILE=spore-host-infra aws lambda get-function \
  --function-name spawn-sweep-orchestrator
```

**Common causes:**
- Lambda timeout (13min limit before re-invoke)
- DynamoDB throttling
- Cross-account role permissions
- All instances at max concurrent

**Solution:**
- Check Lambda logs for errors
- Verify IAM permissions
- Resume sweep to re-invoke Lambda

### Resume Fails

**Symptom:** `spawn resume` returns error

**Check:**
```bash
# For CLI sweeps:
ls ~/.spawn/sweeps/
cat ~/.spawn/sweeps/<sweep-id>.json

# For detached sweeps:
spawn status --sweep-id <id>
```

**Common causes:**
- Sweep already COMPLETED
- State file corrupted/missing
- DynamoDB record not found

**Solution:**
- For CLI sweeps: Check `~/.spawn/sweeps/` directory
- For detached: Query DynamoDB directly or check sweep ID spelling

### Parameter Validation Warnings

**Symptom:** "Parameter validation skipped" warning

**Explanation:**
Validation requires cross-account access. If CLI can't assume the cross-account role, it skips validation. Lambda will validate during orchestration.

**Not a blocker** - sweep will still launch. Validation failures will appear as failed launches.

### Cost Concerns

**Check estimated cost:**
```bash
# Coming in v0.6.0
spawn launch --param-file sweep.json --estimate-only
```

**Track actual cost:**
```bash
spawn status --sweep-id <id>
# Shows estimated vs actual cost (future feature)
```

**Reduce cost:**
- Use Spot instances (`"spot": true` in defaults)
- Set aggressive TTL (`"ttl": "30m"`)
- Use smaller instance types for testing
- Use `--max-concurrent` to limit parallel spending

---

## Best Practices

### File Organization

```bash
sweeps/
├── hyperparams/
│   ├── lr-search-2024-01.json
│   ├── batch-size-test.json
│   └── optimizer-comparison.json
├── multiregion/
│   └── latency-test.json
└── production/
    └── final-config.json
```

### Naming Convention

Use descriptive names:
```json
{
  "params": [
    {"name": "prod-us-east-baseline-2024-01-16", ...},
    {"name": "test-gpu-model-v2-attempt-3", ...}
  ]
}
```

### Start Small

Test with 2-3 instances before full sweep:

```json
{
  "params": [
    {"name": "test-1", ...},
    {"name": "test-2", ...}
    // Uncomment after testing:
    // {"name": "test-3", ...},
    // {"name": "test-4", ...}
  ]
}
```

### Use Detached Mode for Long Sweeps

**Rule of thumb:**
- Sweep < 30 min → CLI-orchestrated is fine
- Sweep > 30 min → Use `--detach`

### Set Reasonable TTLs

Don't rely on manual termination:

```json
{
  "defaults": {
    "ttl": "2h",           // Hard limit
    "idle_timeout": "15m"  // Terminate if idle
  }
}
```

### Monitor Progress

For detached sweeps:

```bash
# Check periodically
spawn status --sweep-id <id>

# Or set up auto-refresh
watch -n 30 spawn status --sweep-id <id>
```

### Document Your Sweeps

Keep notes alongside parameter files:

```bash
cat > sweep.json << 'EOF'
{
  "comment": "Testing learning rates for ResNet-50 on ImageNet",
  "date": "2024-01-16",
  "defaults": {...},
  "params": [...]
}
EOF
```

### Version Parameter Files

Use git to track changes:

```bash
git add sweeps/hyperparam-v1.json
git commit -m "Initial hyperparameter sweep config"
```

---

## Next Steps

- **Architecture details:** See [DETACHED_MODE.md](DETACHED_MODE.md)
- **Report issues:** [GitHub Issues](https://github.com/spore-host/spore-host/issues)
- **Feature requests:** [Issue #23 (Dashboard)](https://github.com/spore-host/spore-host/issues/23), [#24 (Multi-region)](https://github.com/spore-host/spore-host/issues/24), [#25 (Cost estimation)](https://github.com/spore-host/spore-host/issues/25)

---

**Questions or feedback?** Open an issue on GitHub!
