# Queue Configuration Reference

Complete reference for batch job queue configuration files.

## File Format

Queue configurations use JSON format to define sequential job execution.

## Basic Structure

```json
{
  "queue_id": "pipeline-20260127",
  "jobs": [
    {
      "job_id": "preprocess",
      "command": "python preprocess.py",
      "retry_count": 3,
      "timeout": "30m"
    },
    {
      "job_id": "train",
      "command": "python train.py",
      "depends_on": ["preprocess"],
      "retry_count": 1,
      "timeout": "4h"
    }
  ]
}
```

## Top-Level Fields

### queue_id
**Type:** String
**Required:** No
**Default:** Auto-generated
**Description:** Unique queue identifier.

```json
{
  "queue_id": "ml-pipeline-20260127"
}
```

### jobs
**Type:** Array of job objects
**Required:** Yes
**Description:** Sequential jobs to execute.

## Job Object Fields

### job_id
**Type:** String
**Required:** Yes
**Description:** Unique job identifier within queue.

```json
{
  "job_id": "preprocess"
}
```

### command
**Type:** String
**Required:** Yes
**Description:** Command to execute.

```json
{
  "command": "python train.py --data /data --output /results"
}
```

### depends_on
**Type:** Array of strings
**Required:** No
**Default:** `[]` (no dependencies)
**Description:** Job IDs that must complete before this job starts.

```json
{
  "job_id": "train",
  "depends_on": ["preprocess", "validate"]
}
```

### retry_count
**Type:** Integer
**Required:** No
**Default:** `0` (no retries)
**Description:** Number of retry attempts on failure.

```json
{
  "retry_count": 3
}
```

### timeout
**Type:** Duration string
**Required:** No
**Default:** None (no timeout)
**Description:** Maximum execution time.

```json
{
  "timeout": "4h"
}
```

**Format:** `30m`, `2h`, `1d`

### result_files
**Type:** Array of strings
**Required:** No
**Default:** `[]`
**Description:** Files to upload to S3 after job completes.

```json
{
  "result_files": [
    "/data/processed/cleaned_data.csv",
    "/models/model.pth",
    "/results/*.json"
  ]
}
```

**Supports:**
- Absolute paths
- Glob patterns
- Directories (uploaded recursively)

### environment
**Type:** Object
**Required:** No
**Default:** `{}`
**Description:** Environment variables for job.

```json
{
  "environment": {
    "CUDA_VISIBLE_DEVICES": "0",
    "PYTHONPATH": "/app",
    "MODEL_DIR": "/models"
  }
}
```

### working_directory
**Type:** String
**Required:** No
**Default:** Current directory
**Description:** Working directory for command execution.

```json
{
  "working_directory": "/app"
}
```

## Complete Example

```json
{
  "queue_id": "ml-pipeline-20260127",
  "jobs": [
    {
      "job_id": "download",
      "command": "aws s3 sync s3://datasets/cifar10 /data/raw",
      "retry_count": 3,
      "timeout": "15m",
      "result_files": [
        "/data/raw/manifest.json"
      ]
    },
    {
      "job_id": "preprocess",
      "command": "python preprocess.py --input /data/raw --output /data/processed",
      "depends_on": ["download"],
      "retry_count": 2,
      "timeout": "30m",
      "environment": {
        "PYTHONPATH": "/app"
      },
      "result_files": [
        "/data/processed/",
        "/data/processed/stats.json"
      ]
    },
    {
      "job_id": "train",
      "command": "python train.py --data /data/processed --epochs 100 --output /models",
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
      "command": "python evaluate.py --model /models/model.pth --data /data/test --output /results",
      "depends_on": ["train"],
      "retry_count": 2,
      "timeout": "1h",
      "result_files": [
        "/results/eval_results.json",
        "/results/confusion_matrix.png"
      ]
    },
    {
      "job_id": "export",
      "command": "python export.py --model /models/model.pth --format onnx --output /exports/model.onnx",
      "depends_on": ["evaluate"],
      "retry_count": 1,
      "timeout": "30m",
      "result_files": [
        "/exports/model.onnx"
      ]
    },
    {
      "job_id": "upload",
      "command": "aws s3 sync /exports s3://models/trained/$(date +%Y%m%d)/",
      "depends_on": ["export"],
      "retry_count": 3,
      "timeout": "15m"
    }
  ]
}
```

## Execution Order

Jobs execute based on dependencies:

```
download
   ↓
preprocess
   ↓
train
   ↓
evaluate
   ↓
export
   ↓
upload
```

**Parallel execution** possible when no dependencies:

```
download-images     download-labels
       ↓                   ↓
       └──────────┬────────┘
                  ↓
              preprocess
```

## Retry Strategy

### Exponential Backoff

Retries use exponential backoff:
- 1st retry: 10 seconds
- 2nd retry: 30 seconds
- 3rd retry: 90 seconds

### Retry Conditions

Jobs retry on:
- Non-zero exit code
- Timeout exceeded
- System errors (OOM, etc.)

Jobs **don't** retry on:
- File not found (missing dependencies)
- Permission denied
- Dependency failure

## Result Collection

### Automatic Upload

`result_files` automatically upload to S3 after job completion:

```json
{
  "result_files": [
    "/models/model.pth"
  ]
}
```

**Uploaded to:** `s3://spawn-results-<region>/queue-<id>/<job-id>/model.pth`

### Glob Patterns

```json
{
  "result_files": [
    "/results/*.json",
    "/models/**/*.pth",
    "/logs/train-*.log"
  ]
}
```

## Best Practices

### 1. Set Timeouts
```json
{
  "timeout": "4h"
}
```

### 2. Use Retries for Transient Failures
```json
{
  "job_id": "download",
  "retry_count": 3  // Network may be flaky
}
```

### 3. Minimize Dependencies
```json
// Good - parallel when possible
{
  "job_id": "validate",
  "depends_on": ["preprocess"]
}

// Bad - unnecessary dependency
{
  "job_id": "validate",
  "depends_on": ["preprocess", "download"]
}
```

### 4. Specific Result Files
```json
// Good - specific files
{
  "result_files": [
    "/models/model.pth",
    "/results/metrics.json"
  ]
}

// Bad - entire filesystem
{
  "result_files": ["/"]
}
```

## Validation

spawn validates queue configs:

- ✅ All job IDs unique
- ✅ All dependencies exist
- ✅ No circular dependencies
- ✅ Valid timeout formats
- ⚠️ Warns on large result_files

## See Also

- [spawn launch](commands/launch.md) - Launch with queue
- [spawn queue](commands/queue.md) - Monitor queues
