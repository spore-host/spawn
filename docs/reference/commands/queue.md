# spawn queue

Monitor and manage batch job queues running on EC2 instances.

## Synopsis

```bash
spawn queue status <instance-id> [flags]
spawn queue results <queue-id> [flags]
```

## Description

Manage batch job queues that execute sequential jobs with dependency management, retry logic, and result collection. Queues run on single EC2 instances and handle multi-step pipelines (preprocessing → training → evaluation → export).

**Key Features:**
- Sequential job execution
- Job dependencies (blockedBy, blocks)
- Retry strategies with exponential backoff
- Timeout enforcement per job
- Automatic result upload to S3
- Resume after failures
- Real-time status monitoring

## Subcommands

### status
Check queue execution status on an instance.

### results
Download all results from a completed queue.

## status - Check Queue Status

### Synopsis
```bash
spawn queue status <instance-id> [flags]
```

### Arguments

#### instance-id
**Type:** String
**Required:** Yes
**Description:** EC2 instance ID running the queue.

```bash
spawn queue status i-0123456789abcdef0
```

### Flags

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output in JSON format.

```bash
spawn queue status i-1234567890 --json
```

#### --watch, -w
**Type:** Boolean
**Default:** `false`
**Description:** Continuously update status (refresh every 10 seconds).

```bash
spawn queue status i-1234567890 --watch
```

#### --refresh
**Type:** Duration
**Default:** `10s`
**Description:** Refresh interval for `--watch` mode.

```bash
spawn queue status i-1234567890 --watch --refresh 5s
```

### Output

```
Queue: queue-20260127-140532
Instance: i-0123456789abcdef0
Status: running
Started: 2026-01-27 14:05:32 PST
Elapsed: 1h 23m

Jobs: 4 total, 1 completed, 1 running, 2 pending

Job Details:
  ✓ preprocess       completed    Duration: 15m 32s    Attempts: 1/3    Exit: 0
  ▶ train            running      Started: 15:21:04    Attempts: 1/1    PID: 12345
    └─ Progress: Epoch 25/100 (25%)
    └─ Time remaining: ~45m (est)
  ⏸ evaluate         pending      Blocked by: train    Attempts: 0/3
  ⏸ export           pending      Blocked by: evaluate Attempts: 0/2

Current Job (train):
  Command: python train.py --data /data --epochs 100
  Started: 2026-01-27 15:21:04 PST
  Elapsed: 34m 28s
  Timeout: 4h (3h 25m remaining)
  Retry: 1/1
  PID: 12345
  Log: /var/log/spawn-queue/train.log

  Recent Output:
    [15:45:12] Epoch 25/100
    [15:45:12] Train Loss: 0.234
    [15:45:12] Val Loss: 0.256
    [15:45:12] Accuracy: 89.3%

Results Location: s3://spawn-results-us-east-1/queue-20260127-140532/

Next Job: evaluate (after train completes)
Estimated Completion: 2026-01-27 17:30:00 PST (1h 15m)
```

### JSON Output

```json
{
  "queue_id": "queue-20260127-140532",
  "instance_id": "i-0123456789abcdef0",
  "status": "running",
  "started_at": "2026-01-27T14:05:32-08:00",
  "elapsed": "1h23m",
  "jobs": {
    "total": 4,
    "completed": 1,
    "running": 1,
    "pending": 2,
    "failed": 0
  },
  "job_details": [
    {
      "job_id": "preprocess",
      "status": "completed",
      "duration": "15m32s",
      "attempts": 1,
      "max_attempts": 3,
      "exit_code": 0
    },
    {
      "job_id": "train",
      "status": "running",
      "started_at": "2026-01-27T15:21:04-08:00",
      "elapsed": "34m28s",
      "timeout": "4h",
      "timeout_remaining": "3h25m",
      "attempts": 1,
      "max_attempts": 1,
      "pid": 12345,
      "log_file": "/var/log/spawn-queue/train.log"
    }
  ],
  "results_location": "s3://spawn-results-us-east-1/queue-20260127-140532/",
  "estimated_completion": "2026-01-27T17:30:00-08:00"
}
```

## results - Download Queue Results

### Synopsis
```bash
spawn queue results <queue-id> [flags]
```

### Arguments

#### queue-id
**Type:** String
**Required:** Yes
**Description:** Queue ID to download results from.

```bash
spawn queue results queue-20260127-140532
```

### Flags

#### --output, -o
**Type:** Path
**Default:** `./results/<queue-id>/`
**Description:** Local directory to download results to.

```bash
spawn queue results queue-20260127-140532 --output ./my-results/
```

#### --job
**Type:** String
**Default:** All jobs
**Description:** Download results for specific job only.

```bash
spawn queue results queue-20260127-140532 --job train
```

#### --logs-only
**Type:** Boolean
**Default:** `false`
**Description:** Download logs only (no result files).

```bash
spawn queue results queue-20260127-140532 --logs-only
```

#### --no-logs
**Type:** Boolean
**Default:** `false`
**Description:** Download result files only (no logs).

```bash
spawn queue results queue-20260127-140532 --no-logs
```

### Output

```
Downloading results for queue: queue-20260127-140532

Source: s3://spawn-results-us-east-1/queue-20260127-140532/
Destination: ./results/queue-20260127-140532/

Jobs:
  ✓ preprocess (2.3 MB)
    ├─ stdout.log (145 KB)
    ├─ stderr.log (8 KB)
    └─ results/
        ├─ cleaned_data.csv (2.1 MB)
        └─ stats.json (4 KB)

  ✓ train (256 MB)
    ├─ stdout.log (892 KB)
    ├─ stderr.log (12 KB)
    └─ results/
        ├─ model.pth (245 MB)
        ├─ metrics.json (8 KB)
        └─ checkpoints/ (11 MB)

  ✓ evaluate (512 KB)
    ├─ stdout.log (234 KB)
    ├─ stderr.log (4 KB)
    └─ results/
        ├─ eval_results.json (12 KB)
        └─ confusion_matrix.png (256 KB)

  ✓ export (128 MB)
    ├─ stdout.log (67 KB)
    ├─ stderr.log (2 KB)
    └─ results/
        └─ model_export.onnx (128 MB)

Total downloaded: 387 MB
Time: 45 seconds

Results saved to: ./results/queue-20260127-140532/
```

### Directory Structure

```
results/queue-20260127-140532/
├── preprocess/
│   ├── stdout.log
│   ├── stderr.log
│   ├── exit_code
│   ├── metadata.json
│   └── results/
│       ├── cleaned_data.csv
│       └── stats.json
├── train/
│   ├── stdout.log
│   ├── stderr.log
│   ├── exit_code
│   ├── metadata.json
│   └── results/
│       ├── model.pth
│       ├── metrics.json
│       └── checkpoints/
├── evaluate/
│   └── ...
└── export/
    └── ...
```

## Examples

### Launch Queue and Monitor
```bash
# Launch instance with queue
spawn launch --instance-type m7i.large --batch-queue pipeline.json

# Get instance ID
INSTANCE=$(spawn list --quiet | head -1)

# Monitor queue status
spawn queue status "$INSTANCE"

# Watch in real-time
spawn queue status "$INSTANCE" --watch
```

### Check Specific Job
```bash
# Get queue ID from status
QUEUE_ID=$(spawn queue status "$INSTANCE" --json | jq -r '.queue_id')

# Check job status
spawn queue status "$INSTANCE" --json | \
  jq '.job_details[] | select(.job_id == "train")'
```

### Download Results
```bash
# After queue completes
QUEUE_ID="queue-20260127-140532"

# Download all results
spawn queue results "$QUEUE_ID" --output ./my-results/

# Download specific job
spawn queue results "$QUEUE_ID" --job train --output ./train-results/

# Logs only
spawn queue results "$QUEUE_ID" --logs-only
```

### Monitor Queue in Script
```bash
#!/bin/bash
INSTANCE_ID="i-0123456789abcdef0"

while true; do
    STATUS=$(spawn queue status "$INSTANCE_ID" --json)
    QUEUE_STATUS=$(echo "$STATUS" | jq -r '.status')

    if [[ "$QUEUE_STATUS" == "completed" ]]; then
        echo "Queue completed successfully!"
        QUEUE_ID=$(echo "$STATUS" | jq -r '.queue_id')
        spawn queue results "$QUEUE_ID"
        break
    elif [[ "$QUEUE_STATUS" == "failed" ]]; then
        echo "Queue failed!"
        FAILED_JOB=$(echo "$STATUS" | jq -r '.job_details[] | select(.status == "failed") | .job_id')
        echo "Failed job: $FAILED_JOB"
        exit 1
    fi

    COMPLETED=$(echo "$STATUS" | jq -r '.jobs.completed')
    TOTAL=$(echo "$STATUS" | jq -r '.jobs.total')
    echo "Progress: $COMPLETED / $TOTAL"

    sleep 30
done
```

## Queue Configuration

Queues are defined in JSON configuration files. See [Queue Configs](../queue-configs.md) for complete reference.

**Example pipeline.json:**
```json
{
  "queue_id": "pipeline-20260127",
  "jobs": [
    {
      "job_id": "preprocess",
      "command": "python preprocess.py --input /data/raw --output /data/processed",
      "retry_count": 3,
      "timeout": "30m",
      "result_files": [
        "/data/processed/cleaned_data.csv",
        "/data/processed/stats.json"
      ]
    },
    {
      "job_id": "train",
      "command": "python train.py --data /data/processed --epochs 100 --output /models",
      "retry_count": 1,
      "timeout": "4h",
      "depends_on": ["preprocess"],
      "result_files": [
        "/models/model.pth",
        "/models/metrics.json"
      ]
    },
    {
      "job_id": "evaluate",
      "command": "python evaluate.py --model /models/model.pth --data /data/test --output /results",
      "retry_count": 3,
      "timeout": "1h",
      "depends_on": ["train"],
      "result_files": [
        "/results/eval_results.json",
        "/results/confusion_matrix.png"
      ]
    },
    {
      "job_id": "export",
      "command": "python export.py --model /models/model.pth --output /exports/model.onnx",
      "retry_count": 2,
      "timeout": "30m",
      "depends_on": ["evaluate"],
      "result_files": [
        "/exports/model.onnx"
      ]
    }
  ]
}
```

## Job States

- `pending` - Waiting for dependencies
- `running` - Currently executing
- `completed` - Finished successfully (exit code 0)
- `failed` - Failed after all retries
- `retrying` - Failed, will retry
- `blocked` - Waiting for another job to complete

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Operation successful |
| 1 | Operation failed (instance not found, queue error) |
| 2 | Invalid arguments (missing instance ID or queue ID) |
| 3 | Queue not found (instance has no queue or queue doesn't exist) |

## Troubleshooting

### "Queue not found"
```bash
# Check if instance has queue
spawn list --format json | jq '.[] | select(.instance_id == "i-1234567890") | .tags'

# Check instance status
spawn status i-1234567890
```

### Job Stuck in "running"
```bash
# Check job logs on instance
spawn connect i-1234567890 -c "tail -f /var/log/spawn-queue/train.log"

# Check if process still running
spawn connect i-1234567890 -c "ps aux | grep python"

# Kill stuck job (will retry if retries remaining)
spawn connect i-1234567890 -c "kill <PID>"
```

### Results Not Uploading
```bash
# Check S3 permissions (IAM role)
spawn status i-1234567890 | grep "IAM"

# Check network connectivity
spawn connect i-1234567890 -c "aws s3 ls"

# Manual upload
spawn connect i-1234567890 -c "aws s3 sync /tmp/results/ s3://spawn-results-us-east-1/queue-xxx/"
```

### Failed Job
```bash
# Check job logs
spawn queue status i-1234567890 --json | jq '.job_details[] | select(.status == "failed")'

# Download logs
spawn queue results queue-xxx --job failed-job --logs-only

# Check exit code
cat results/queue-xxx/failed-job/exit_code
```

## Performance

- **Status Check:** ~500ms (SSH + file read)
- **Results Download:** Depends on size (~10 MB/s typical)
- **Watch Mode:** Minimal overhead (cached between refreshes)

## See Also
- [spawn launch](launch.md) - Launch with batch queue
- [spawn status](status.md) - Check instance status
- [Queue Configs](../queue-configs.md) - Queue configuration format
- [IAM Policies](../iam-policies.md) - Required S3 permissions
