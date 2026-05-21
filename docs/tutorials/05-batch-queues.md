# Tutorial 5: Batch Queues

**Duration:** 45 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 4: Job Arrays](04-job-arrays.md)

## What You'll Learn

In this tutorial, you'll learn how to run sequential batch jobs with dependencies:
- Create queue configuration files
- Define job dependencies (DAGs)
- Set retry strategies and timeouts
- Monitor queue execution
- Collect results from completed pipelines
- Build ML training pipelines
- Handle failures and retries

## Batch Queues vs Job Arrays

| Feature | Batch Queues | Job Arrays |
|---------|-------------|------------|
| **Execution** | Sequential (one after another) | Parallel (all at once) |
| **Dependencies** | Supported (job B waits for job A) | Independent |
| **Use Case** | Pipelines (download → process → train → evaluate) | Parallel batch (100 files) |
| **Instances** | Single instance runs all jobs | Multiple instances (one per job) |
| **Retries** | Per-job retry logic | Manual retry needed |

**When to use batch queues:**
- ML training pipelines (preprocess → train → evaluate)
- Data pipelines (extract → transform → load)
- Build processes (compile → test → package → deploy)
- Any multi-step workflow with dependencies

**When to use job arrays:**
- Process independent files in parallel
- Hyperparameter sweeps
- Monte Carlo simulations

## Understanding Batch Queues

A batch queue consists of:

1. **Queue Configuration** - JSON file defining jobs and dependencies
2. **Queue Runner** - Runs on single EC2 instance
3. **Job Dependencies** - DAG (Directed Acyclic Graph) of jobs
4. **Retry Logic** - Automatic retries with exponential backoff
5. **Result Collection** - Uploads to S3 after each job

**Key Concept:** Jobs execute sequentially based on dependencies.

**Example Pipeline:**
```
download → preprocess → train → evaluate → export
```

Each job waits for previous job to complete successfully.

## Your First Batch Queue

Let's create a simple 3-job pipeline.

### Step 1: Create Queue Configuration

```bash
cd ~
cat > simple-queue.json << 'EOF'
{
  "queue_id": "first-pipeline",
  "jobs": [
    {
      "job_id": "step1",
      "command": "echo 'Step 1: Download data' && sleep 10",
      "retry_count": 2,
      "timeout": "1m"
    },
    {
      "job_id": "step2",
      "command": "echo 'Step 2: Process data' && sleep 10",
      "depends_on": ["step1"],
      "retry_count": 2,
      "timeout": "1m"
    },
    {
      "job_id": "step3",
      "command": "echo 'Step 3: Upload results' && sleep 10",
      "depends_on": ["step2"],
      "retry_count": 3,
      "timeout": "1m"
    }
  ]
}
EOF
```

**What this defines:**
- 3 sequential jobs
- Each depends on previous job
- Retry logic (2-3 attempts)
- Timeouts (1 minute per job)

### Step 2: Launch Queue

```bash
spawn launch \
  --instance-type t3.micro \
  --ttl 30m \
  --name simple-queue \
  --batch-queue simple-queue.json
```

**Expected output:**
```
🚀 Launching batch queue...

Queue: first-pipeline
Jobs: 3
Instance Type: t3.micro
TTL: 30m

Progress:
  ✓ Validating queue configuration
  ✓ Launching instance
  ✓ Installing queue runner
  ✓ Starting queue execution

Instance launched! 🎉

Instance ID: i-0abc123def456789
Queue ID: first-pipeline

Monitor:
  spawn queue status i-0abc123def456789

Estimated cost: ~$0.005 (30 minutes × $0.0104/hour)
Estimated runtime: ~30 seconds (3 jobs × ~10s each)
```

### Step 3: Monitor Queue

```bash
spawn queue status i-0abc123def456789
```

**Expected output (while running):**
```
Queue: first-pipeline
Instance: i-0abc123def456789
Status: running
Started: 2026-01-27 10:00:00 PST
Elapsed: 15s

Jobs: 3 total, 1 completed, 1 running, 1 pending

Job Details:
  ✓ step1   completed   Duration: 10s   Attempts: 1/2   Exit: 0
  ▶ step2   running     Elapsed: 5s     Attempts: 1/2   PID: 1234
  ⏸ step3   pending     Blocked by: step2

Current Job (step2):
  Command: echo 'Step 2: Process data' && sleep 10
  Started: 2026-01-27 10:00:10 PST
  Elapsed: 5s
  Timeout: 1m (55s remaining)
  Log: /var/log/spawn-queue/step2.log

Next Job: step3 (after step2 completes)
```

**Watch mode:**
```bash
spawn queue status i-0abc123def456789 --watch
```

Auto-refreshes every 10 seconds until queue completes.

### Step 4: After Completion

```bash
spawn queue status i-0abc123def456789
```

**Expected output (completed):**
```
Queue: first-pipeline
Instance: i-0abc123def456789
Status: completed
Started: 2026-01-27 10:00:00 PST
Completed: 2026-01-27 10:00:30 PST
Duration: 30s

Jobs: 3 total, 3 completed, 0 running, 0 pending

Job Details:
  ✓ step1   completed   Duration: 10s   Attempts: 1/2   Exit: 0
  ✓ step2   completed   Duration: 10s   Attempts: 1/2   Exit: 0
  ✓ step3   completed   Duration: 10s   Attempts: 1/3   Exit: 0

All jobs completed successfully! 🎉
```

## Real-World Example: ML Training Pipeline

Let's build a complete ML training pipeline.

### Step 1: Create Pipeline Configuration

```bash
cat > ml-pipeline.json << 'EOF'
{
  "queue_id": "ml-training-20260127",
  "jobs": [
    {
      "job_id": "download",
      "command": "python scripts/download_data.py --dataset cifar10 --output /data/raw",
      "retry_count": 3,
      "timeout": "15m",
      "result_files": [
        "/data/raw/manifest.json"
      ]
    },
    {
      "job_id": "preprocess",
      "command": "python scripts/preprocess.py --input /data/raw --output /data/processed",
      "depends_on": ["download"],
      "retry_count": 2,
      "timeout": "30m",
      "environment": {
        "PYTHONPATH": "/app"
      },
      "result_files": [
        "/data/processed/train.tfrecord",
        "/data/processed/val.tfrecord",
        "/data/processed/stats.json"
      ]
    },
    {
      "job_id": "train",
      "command": "python scripts/train.py --data /data/processed --epochs 100 --output /models",
      "depends_on": ["preprocess"],
      "retry_count": 1,
      "timeout": "4h",
      "environment": {
        "CUDA_VISIBLE_DEVICES": "0",
        "WANDB_API_KEY": "${WANDB_API_KEY}"
      },
      "result_files": [
        "/models/model.pth",
        "/models/metrics.json",
        "/models/checkpoints/"
      ]
    },
    {
      "job_id": "evaluate",
      "command": "python scripts/evaluate.py --model /models/model.pth --data /data/test --output /results",
      "depends_on": ["train"],
      "retry_count": 2,
      "timeout": "1h",
      "result_files": [
        "/results/eval_results.json",
        "/results/confusion_matrix.png",
        "/results/predictions.csv"
      ]
    },
    {
      "job_id": "export",
      "command": "python scripts/export.py --model /models/model.pth --format onnx --output /exports/model.onnx",
      "depends_on": ["evaluate"],
      "retry_count": 1,
      "timeout": "30m",
      "result_files": [
        "/exports/model.onnx",
        "/exports/model_info.json"
      ]
    }
  ]
}
EOF
```

**Pipeline:**
```
download (15m)
   ↓
preprocess (30m)
   ↓
train (4h)
   ↓
evaluate (1h)
   ↓
export (30m)

Total: ~6h 15m
```

### Step 2: Prepare Scripts

Create a simple train script:

```bash
mkdir -p scripts
cat > scripts/train.py << 'EOF'
#!/usr/bin/env python3
import os
import time
import json
import random

print("Starting training...")
print(f"Data directory: {os.sys.argv[2]}")
print(f"Epochs: {os.sys.argv[4]}")

# Simulate training
for epoch in range(1, 11):  # Simplified to 10 epochs for demo
    time.sleep(5)
    loss = 2.0 / (epoch + 1) + random.random() * 0.1
    acc = min(0.95, 0.5 + epoch * 0.04)
    print(f"Epoch {epoch}/10 - Loss: {loss:.4f} - Accuracy: {acc:.4f}")

# Save model and metrics
os.makedirs('/models', exist_ok=True)
with open('/models/model.pth', 'w') as f:
    f.write('model_weights_placeholder')

metrics = {
    'final_loss': 0.234,
    'final_accuracy': 0.923,
    'epochs': 10
}
with open('/models/metrics.json', 'w') as f:
    json.dump(metrics, f, indent=2)

print("Training complete!")
EOF

chmod +x scripts/train.py
```

### Step 3: Launch Pipeline

```bash
spawn launch \
  --instance-type g5.xlarge \
  --ttl 8h \
  --name ml-pipeline \
  --iam-policy s3:FullAccess,logs:WriteOnly \
  --on-complete terminate \
  --batch-queue ml-pipeline.json \
  --wait-for-ssh
```

**What this does:**
- Launches g5.xlarge (GPU instance)
- Sets 8h TTL (pipeline takes ~6h 15m)
- Configures IAM for S3 access
- Auto-terminates when pipeline completes
- Waits for SSH before starting queue

**Expected output:**
```
🚀 Launching ML training pipeline...

Queue: ml-training-20260127
Jobs: 5
Instance Type: g5.xlarge
TTL: 8h

Pipeline:
  download (15m)
    ↓
  preprocess (30m)
    ↓
  train (4h)
    ↓
  evaluate (1h)
    ↓
  export (30m)

Estimated Duration: 6h 15m
Estimated Cost: ~$7.00 (g5.xlarge for 8h)

Launching...
  ✓ Instance launched: i-0abc123def456789
  ✓ Queue runner installed
  ✓ Pipeline execution started

Monitor:
  spawn queue status i-0abc123def456789 --watch

Results will upload to: s3://spawn-results-us-east-1/ml-training-20260127/
```

### Step 4: Monitor Progress

```bash
# Real-time monitoring
spawn queue status i-0abc123def456789 --watch

# Or periodic checks
spawn queue status i-0abc123def456789
```

**Output during training phase:**
```
Queue: ml-training-20260127
Instance: i-0abc123def456789
Status: running
Started: 2026-01-27 08:00:00 PST
Elapsed: 2h 45m

Jobs: 5 total, 2 completed, 1 running, 2 pending

Job Details:
  ✓ download     completed   Duration: 14m 23s   Attempts: 1/3   Exit: 0
  ✓ preprocess   completed   Duration: 28m 12s   Attempts: 1/2   Exit: 0
  ▶ train        running     Elapsed: 2h 2m      Attempts: 1/1   PID: 5678
    └─ Progress: Epoch 52/100 (52%)
    └─ Time remaining: ~1h 56m (est)
  ⏸ evaluate     pending     Blocked by: train
  ⏸ export       pending     Blocked by: evaluate

Current Job (train):
  Command: python scripts/train.py --data /data/processed --epochs 100 --output /models
  Started: 2026-01-27 08:42:35 PST
  Elapsed: 2h 2m
  Timeout: 4h (1h 58m remaining)
  PID: 5678
  Log: /var/log/spawn-queue/train.log

  Recent Output:
    [10:45:12] Epoch 52/100
    [10:45:12] Train Loss: 0.287
    [10:45:12] Val Loss: 0.301
    [10:45:12] Accuracy: 87.6%
    [10:45:12] Saving checkpoint...

Results Location: s3://spawn-results-us-east-1/ml-training-20260127/

Next Job: evaluate (after train completes)
Estimated Completion: 2026-01-27 14:15:00 PST
```

### Step 5: Collect Results

After pipeline completes:

```bash
spawn queue results ml-training-20260127 --output ./ml-results/
```

**Expected output:**
```
Downloading results from queue ml-training-20260127...

  ✓ download/manifest.json
  ✓ preprocess/train.tfrecord (2.3 GB)
  ✓ preprocess/val.tfrecord (512 MB)
  ✓ preprocess/stats.json
  ✓ train/model.pth (1.2 GB)
  ✓ train/metrics.json
  ✓ train/checkpoints/ (5.6 GB)
  ✓ evaluate/eval_results.json
  ✓ evaluate/confusion_matrix.png
  ✓ evaluate/predictions.csv
  ✓ export/model.onnx (1.3 GB)
  ✓ export/model_info.json

Downloaded: 11.8 GB
Output directory: ./ml-results/

Results structure:
./ml-results/
  ├── download/
  │   └── manifest.json
  ├── preprocess/
  │   ├── train.tfrecord
  │   ├── val.tfrecord
  │   └── stats.json
  ├── train/
  │   ├── model.pth
  │   ├── metrics.json
  │   └── checkpoints/
  ├── evaluate/
  │   ├── eval_results.json
  │   ├── confusion_matrix.png
  │   └── predictions.csv
  └── export/
      ├── model.onnx
      └── model_info.json
```

## Real-World Example: Data ETL Pipeline

Extract, transform, load pipeline with retries.

### Create ETL Queue

```bash
cat > etl-pipeline.json << 'EOF'
{
  "queue_id": "etl-20260127",
  "jobs": [
    {
      "job_id": "extract",
      "command": "python etl/extract.py --source postgres://db/prod --output /data/raw",
      "retry_count": 5,
      "timeout": "1h",
      "environment": {
        "DB_PASSWORD": "${DB_PASSWORD}"
      },
      "result_files": [
        "/data/raw/manifest.json",
        "/data/raw/*.parquet"
      ]
    },
    {
      "job_id": "transform",
      "command": "python etl/transform.py --input /data/raw --output /data/transformed",
      "depends_on": ["extract"],
      "retry_count": 3,
      "timeout": "2h",
      "result_files": [
        "/data/transformed/*.parquet",
        "/data/transformed/schema.json"
      ]
    },
    {
      "job_id": "validate",
      "command": "python etl/validate.py --data /data/transformed",
      "depends_on": ["transform"],
      "retry_count": 1,
      "timeout": "30m",
      "result_files": [
        "/validation/report.html",
        "/validation/issues.json"
      ]
    },
    {
      "job_id": "load",
      "command": "python etl/load.py --data /data/transformed --target s3://data-warehouse/",
      "depends_on": ["validate"],
      "retry_count": 5,
      "timeout": "1h",
      "environment": {
        "AWS_REGION": "us-east-1"
      }
    },
    {
      "job_id": "notify",
      "command": "curl -X POST $SLACK_WEBHOOK -d '{\"text\": \"ETL pipeline completed!\"}'",
      "depends_on": ["load"],
      "retry_count": 3,
      "timeout": "1m"
    }
  ]
}
EOF
```

**Pipeline with error handling:**
- Extract: 5 retries (network issues common)
- Transform: 3 retries (computation errors)
- Validate: 1 retry (should be deterministic)
- Load: 5 retries (network upload)
- Notify: 3 retries (webhook may fail)

## Advanced: Parallel Jobs

Multiple jobs can run simultaneously if they don't depend on each other.

### Parallel Feature Extraction

```json
{
  "queue_id": "feature-extraction",
  "jobs": [
    {
      "job_id": "download",
      "command": "download_data.sh",
      "timeout": "30m"
    },
    {
      "job_id": "extract_text",
      "command": "extract_text_features.py",
      "depends_on": ["download"],
      "timeout": "1h"
    },
    {
      "job_id": "extract_image",
      "command": "extract_image_features.py",
      "depends_on": ["download"],
      "timeout": "2h"
    },
    {
      "job_id": "extract_audio",
      "command": "extract_audio_features.py",
      "depends_on": ["download"],
      "timeout": "1h 30m"
    },
    {
      "job_id": "merge",
      "command": "merge_features.py",
      "depends_on": ["extract_text", "extract_image", "extract_audio"],
      "timeout": "30m"
    }
  ]
}
```

**Execution:**
```
        download
           ↓
    ┌──────┼──────┐
    ↓      ↓      ↓
  text   image  audio  (parallel)
    └──────┼──────┘
           ↓
         merge
```

`extract_text`, `extract_image`, and `extract_audio` run in parallel after `download` completes.

## Handling Failures

### Automatic Retries

Configure retry strategies per job:

```json
{
  "job_id": "flaky_job",
  "command": "python flaky_script.py",
  "retry_count": 3,
  "timeout": "1h"
}
```

**Retry behavior:**
- Attempt 1 fails → Wait 1 minute → Attempt 2
- Attempt 2 fails → Wait 2 minutes → Attempt 3
- Attempt 3 fails → Wait 4 minutes → Attempt 4
- Attempt 4 fails → Mark job as failed, stop queue

**Exponential backoff:** Wait time doubles between retries.

### Manual Retry

If queue fails, fix issue and resume:

```bash
# Fix the problem (update script, fix permissions, etc.)

# Resume queue from failed job
spawn queue resume i-0abc123def456789
```

### Timeout Handling

Set appropriate timeouts:

```json
{
  "job_id": "quick_task",
  "timeout": "5m"  // Kills job if exceeds 5 minutes
}
```

**Best practice:** Set timeout to 2-3x expected duration.

## Monitoring and Debugging

### Watch Logs in Real-Time

```bash
# Connect to instance
spawn connect i-0abc123def456789

# Tail current job log
tail -f /var/log/spawn-queue/$(cat /var/run/spawn-queue/current-job).log

# Or watch all logs
tail -f /var/log/spawn-queue/*.log
```

### Check Job Status

```bash
# Detailed status
spawn queue status i-0abc123def456789 --json | jq '.job_details'

# Filter completed jobs
spawn queue status i-0abc123def456789 --json | jq '.job_details[] | select(.status == "completed")'
```

### Debug Failed Job

```bash
# View logs for failed job
spawn connect i-0abc123def456789
cat /var/log/spawn-queue/failed_job.log

# Check exit code
cat /var/log/spawn-queue/failed_job.exit
```

## Best Practices

### 1. Set Realistic Timeouts

```json
// Good: 2-3x expected duration
{
  "job_id": "train",
  "command": "python train.py",
  "timeout": "4h"  // Training takes ~2-3 hours
}

// Bad: Too short
{
  "timeout": "30m"  // Training will be killed
}
```

### 2. Use Appropriate Retry Counts

```json
// Network operations: High retry count
{
  "job_id": "download",
  "retry_count": 5
}

// Compute operations: Low retry count
{
  "job_id": "train",
  "retry_count": 1  // If training fails, likely a code bug
}
```

### 3. Upload Results Incrementally

```json
{
  "job_id": "train",
  "result_files": [
    "/models/checkpoints/"  // Upload checkpoints as they're created
  ]
}
```

Don't wait until end to upload everything.

### 4. Use Environment Variables

```json
{
  "environment": {
    "WANDB_API_KEY": "${WANDB_API_KEY}",
    "S3_BUCKET": "my-results"
  }
}
```

Pass secrets via environment variables, not hardcoded in config.

### 5. Validate Dependencies

Ensure DAG is acyclic (no circular dependencies):

```json
// Bad: Circular dependency
{
  "jobs": [
    {
      "job_id": "job_a",
      "depends_on": ["job_b"]
    },
    {
      "job_id": "job_b",
      "depends_on": ["job_a"]  // Circular!
    }
  ]
}
```

### 6. Monitor Costs

```bash
# Check cost periodically
spawn cost --instance-id i-0abc123def456789

# Set alerts
spawn alerts create cost --threshold 10.00
```

## What You Learned

Congratulations! You now understand:

✅ Batch queues vs job arrays
✅ Queue configuration files (JSON)
✅ Job dependencies and DAGs
✅ Retry strategies and timeouts
✅ Monitoring queue execution
✅ ML training pipelines
✅ ETL pipelines
✅ Parallel job execution
✅ Failure handling

## Practice Exercises

### Exercise 1: Simple Pipeline

Create a 3-job pipeline: download → process → upload

### Exercise 2: Parallel Jobs

Create a pipeline where 3 jobs run in parallel after an initial download step.

### Exercise 3: Retry Logic

Create a job that randomly fails 50% of the time. Configure retry logic to ensure it eventually succeeds.

## Next Steps

Continue your learning journey:

📖 **[Tutorial 6: Cost Management](06-cost-management.md)** - Track and optimize costs

🛠️ **[How-To: Batch Queues](../how-to/batch-queues.md)** - Advanced patterns

📚 **[Queue Configuration Reference](../reference/queue-configs.md)** - Complete format documentation

## Quick Reference

```bash
# Launch queue
spawn launch --batch-queue pipeline.json --instance-type m7i.large

# Monitor queue
spawn queue status <instance-id>
spawn queue status <instance-id> --watch

# Collect results
spawn queue results <queue-id> --output ./results/

# Resume failed queue
spawn queue resume <instance-id>

# Connect and debug
spawn connect <instance-id>
tail -f /var/log/spawn-queue/*.log
```

**Queue Configuration:**
```json
{
  "queue_id": "my-pipeline",
  "jobs": [
    {
      "job_id": "step1",
      "command": "command1",
      "retry_count": 3,
      "timeout": "1h",
      "result_files": ["/path/to/results"]
    },
    {
      "job_id": "step2",
      "command": "command2",
      "depends_on": ["step1"],
      "timeout": "2h"
    }
  ]
}
```

---

**Previous:** [← Tutorial 4: Job Arrays](04-job-arrays.md)
**Next:** [Tutorial 6: Cost Management](06-cost-management.md) →
