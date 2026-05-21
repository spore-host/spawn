# How-To: Batch Queues

Advanced patterns for sequential batch job queues.

## Complex DAG Workflows

### Problem
Multi-stage pipeline with parallel branches and dependencies.

### Solution: Complex DAG

```json
{
  "queue_id": "complex-pipeline",
  "jobs": [
    {
      "job_id": "download-data",
      "command": "aws s3 sync s3://source/data/ /data/raw/",
      "retry_count": 5,
      "timeout": "30m"
    },
    {
      "job_id": "preprocess-train",
      "command": "python preprocess.py --split train --input /data/raw --output /data/train",
      "depends_on": ["download-data"],
      "timeout": "1h"
    },
    {
      "job_id": "preprocess-val",
      "command": "python preprocess.py --split val --input /data/raw --output /data/val",
      "depends_on": ["download-data"],
      "timeout": "30m"
    },
    {
      "job_id": "preprocess-test",
      "command": "python preprocess.py --split test --input /data/raw --output /data/test",
      "depends_on": ["download-data"],
      "timeout": "30m"
    },
    {
      "job_id": "train-model",
      "command": "python train.py --data /data/train --output /models",
      "depends_on": ["preprocess-train", "preprocess-val"],
      "timeout": "8h",
      "environment": {
        "CUDA_VISIBLE_DEVICES": "0"
      }
    },
    {
      "job_id": "evaluate",
      "command": "python evaluate.py --model /models/model.pth --data /data/test --output /results",
      "depends_on": ["train-model", "preprocess-test"],
      "timeout": "1h"
    },
    {
      "job_id": "export-onnx",
      "command": "python export.py --model /models/model.pth --format onnx --output /exports/model.onnx",
      "depends_on": ["evaluate"],
      "timeout": "30m"
    },
    {
      "job_id": "upload-artifacts",
      "command": "aws s3 sync /exports/ s3://models/production/$(date +%Y%m%d)/",
      "depends_on": ["export-onnx"],
      "retry_count": 5,
      "timeout": "15m"
    }
  ]
}
```

**Execution flow:**
```
download-data
     ↓
  ┌──┼───┐
  ↓  ↓   ↓
train val test (parallel)
  └─┬┘   │
    ↓    │
  train-model
    ↓    │
evaluate←┘
    ↓
export-onnx
    ↓
upload-artifacts
```

---

## Conditional Execution

### Problem
Only run certain jobs if previous job meets criteria.

### Solution: Exit Code Checks

```json
{
  "queue_id": "conditional-pipeline",
  "jobs": [
    {
      "job_id": "run-tests",
      "command": "pytest tests/ --cov --cov-report=json",
      "timeout": "30m"
    },
    {
      "job_id": "check-coverage",
      "command": "python check_coverage.py /tmp/coverage.json",
      "depends_on": ["run-tests"],
      "timeout": "5m"
    },
    {
      "job_id": "deploy",
      "command": "bash deploy.sh",
      "depends_on": ["check-coverage"],
      "timeout": "15m"
    }
  ]
}
```

**check_coverage.py:**
```python
#!/usr/bin/env python3
import json
import sys

with open('/tmp/coverage.json') as f:
    coverage = json.load(f)

total_coverage = coverage['totals']['percent_covered']

if total_coverage < 80:
    print(f"Coverage {total_coverage}% is below 80% threshold")
    sys.exit(1)  # Fail - stops pipeline

print(f"Coverage {total_coverage}% meets threshold")
sys.exit(0)  # Success - continue to deploy
```

---

## Fan-Out / Fan-In Pattern

### Problem
Process data in parallel, then aggregate results.

### Solution: Multiple Workers, Single Aggregator

```json
{
  "queue_id": "fan-out-fan-in",
  "jobs": [
    {
      "job_id": "split-data",
      "command": "python split_data.py --input /data/large.csv --output /data/chunks/ --num-chunks 10",
      "timeout": "30m"
    },
    {
      "job_id": "process-chunk-0",
      "command": "python process.py /data/chunks/chunk-0.csv /data/processed/result-0.csv",
      "depends_on": ["split-data"],
      "timeout": "2h"
    },
    {
      "job_id": "process-chunk-1",
      "command": "python process.py /data/chunks/chunk-1.csv /data/processed/result-1.csv",
      "depends_on": ["split-data"],
      "timeout": "2h"
    },
    {
      "job_id": "process-chunk-2",
      "command": "python process.py /data/chunks/chunk-2.csv /data/processed/result-2.csv",
      "depends_on": ["split-data"],
      "timeout": "2h"
    },
    {
      "job_id": "process-chunk-3",
      "command": "python process.py /data/chunks/chunk-3.csv /data/processed/result-3.csv",
      "depends_on": ["split-data"],
      "timeout": "2h"
    },
    {
      "job_id": "process-chunk-4",
      "command": "python process.py /data/chunks/chunk-4.csv /data/processed/result-4.csv",
      "depends_on": ["split-data"],
      "timeout": "2h"
    },
    {
      "job_id": "aggregate",
      "command": "python aggregate.py /data/processed/ /data/final-result.csv",
      "depends_on": [
        "process-chunk-0",
        "process-chunk-1",
        "process-chunk-2",
        "process-chunk-3",
        "process-chunk-4"
      ],
      "timeout": "30m"
    },
    {
      "job_id": "upload-result",
      "command": "aws s3 cp /data/final-result.csv s3://results/output.csv",
      "depends_on": ["aggregate"],
      "timeout": "10m"
    }
  ]
}
```

**Execution:**
```
split-data
    ↓
    ├─→ process-chunk-0 ─┐
    ├─→ process-chunk-1 ─┤
    ├─→ process-chunk-2 ─┼─→ aggregate → upload-result
    ├─→ process-chunk-3 ─┤
    └─→ process-chunk-4 ─┘
```

---

## Retry Strategies

### Exponential Backoff with Different Strategies

```json
{
  "queue_id": "retry-strategies",
  "jobs": [
    {
      "job_id": "network-heavy",
      "command": "python download_large_files.py",
      "retry_count": 5,
      "retry_strategy": "exponential",
      "timeout": "1h"
    },
    {
      "job_id": "compute-heavy",
      "command": "python train.py",
      "retry_count": 1,
      "timeout": "4h"
    },
    {
      "job_id": "flaky-api",
      "command": "python call_external_api.py",
      "retry_count": 10,
      "retry_delay": "30s",
      "timeout": "5m"
    }
  ]
}
```

**Retry behaviors:**
- `network-heavy`: Retries with exponential backoff (1s, 2s, 4s, 8s, 16s)
- `compute-heavy`: No retries (compute errors usually not transient)
- `flaky-api`: Many retries with fixed 30s delay

---

## Parallel Job Stages

### Problem
Some jobs can run in parallel but must complete before next stage.

### Solution: Stage-Based Pipeline

```json
{
  "queue_id": "staged-pipeline",
  "jobs": [
    {
      "job_id": "stage1-download",
      "command": "aws s3 sync s3://source/ /data/",
      "timeout": "30m"
    },
    {
      "job_id": "stage2-extract-text",
      "command": "python extract_text.py /data /features/text",
      "depends_on": ["stage1-download"],
      "timeout": "1h"
    },
    {
      "job_id": "stage2-extract-images",
      "command": "python extract_images.py /data /features/images",
      "depends_on": ["stage1-download"],
      "timeout": "2h"
    },
    {
      "job_id": "stage2-extract-audio",
      "command": "python extract_audio.py /data /features/audio",
      "depends_on": ["stage1-download"],
      "timeout": "1h 30m"
    },
    {
      "job_id": "stage3-merge",
      "command": "python merge_features.py /features /merged",
      "depends_on": ["stage2-extract-text", "stage2-extract-images", "stage2-extract-audio"],
      "timeout": "30m"
    },
    {
      "job_id": "stage4-train",
      "command": "python train.py /merged /models",
      "depends_on": ["stage3-merge"],
      "timeout": "8h"
    }
  ]
}
```

**Stages:**
- Stage 1: Download (1 job)
- Stage 2: Feature extraction (3 jobs in parallel)
- Stage 3: Merge (1 job, waits for all stage 2)
- Stage 4: Train (1 job)

---

## Error Handling and Cleanup

### Problem
Need to clean up resources even if pipeline fails.

### Solution: Cleanup Jobs

```json
{
  "queue_id": "pipeline-with-cleanup",
  "jobs": [
    {
      "job_id": "setup",
      "command": "python setup_resources.py",
      "timeout": "10m"
    },
    {
      "job_id": "process",
      "command": "python process_data.py",
      "depends_on": ["setup"],
      "timeout": "4h",
      "on_failure": "cleanup"
    },
    {
      "job_id": "validate",
      "command": "python validate_results.py",
      "depends_on": ["process"],
      "timeout": "30m",
      "on_failure": "cleanup"
    },
    {
      "job_id": "cleanup",
      "command": "python cleanup_resources.py",
      "depends_on": ["validate"],
      "always_run": true,
      "timeout": "10m"
    }
  ]
}
```

**Behavior:**
- If `process` or `validate` fails, jumps to `cleanup`
- `cleanup` runs regardless of success/failure
- Ensures resources cleaned up

---

## Incremental Processing

### Problem
Pipeline runs daily, only want to process new data.

### Solution: Checkpoint-Based Processing

```json
{
  "queue_id": "incremental-pipeline",
  "jobs": [
    {
      "job_id": "find-new-files",
      "command": "python find_new_files.py --since $(cat /state/last-run.txt) --output /tmp/new-files.txt",
      "timeout": "10m"
    },
    {
      "job_id": "process-new",
      "command": "python process_files.py --input /tmp/new-files.txt --output /data/processed",
      "depends_on": ["find-new-files"],
      "timeout": "2h"
    },
    {
      "job_id": "update-checkpoint",
      "command": "date -u +%Y-%m-%dT%H:%M:%SZ > /state/last-run.txt && aws s3 cp /state/last-run.txt s3://state/",
      "depends_on": ["process-new"],
      "timeout": "1m"
    }
  ]
}
```

**find_new_files.py:**
```python
#!/usr/bin/env python3
import sys
from datetime import datetime
import boto3

since_str = sys.argv[sys.argv.index('--since') + 1]
since = datetime.fromisoformat(since_str)

s3 = boto3.client('s3')
response = s3.list_objects_v2(Bucket='data-bucket', Prefix='input/')

new_files = []
for obj in response.get('Contents', []):
    if obj['LastModified'] > since:
        new_files.append(obj['Key'])

with open('/tmp/new-files.txt', 'w') as f:
    for file in new_files:
        f.write(f"{file}\n")

print(f"Found {len(new_files)} new files since {since}")
```

---

## Result Validation

### Problem
Want to validate results before proceeding.

### Solution: Validation Jobs

```json
{
  "queue_id": "validated-pipeline",
  "jobs": [
    {
      "job_id": "train-model",
      "command": "python train.py --output /models/model.pth",
      "timeout": "4h",
      "result_files": ["/models/model.pth", "/models/metrics.json"]
    },
    {
      "job_id": "validate-model",
      "command": "python validate_model.py /models/model.pth",
      "depends_on": ["train-model"],
      "timeout": "30m"
    },
    {
      "job_id": "benchmark",
      "command": "python benchmark.py /models/model.pth /data/test",
      "depends_on": ["validate-model"],
      "timeout": "1h"
    },
    {
      "job_id": "validate-performance",
      "command": "python check_performance.py /results/benchmark.json",
      "depends_on": ["benchmark"],
      "timeout": "5m"
    },
    {
      "job_id": "deploy",
      "command": "python deploy.py /models/model.pth",
      "depends_on": ["validate-performance"],
      "timeout": "15m"
    }
  ]
}
```

**validate_model.py:**
```python
#!/usr/bin/env python3
import torch
import sys

model_path = sys.argv[1]

try:
    # Load model
    model = torch.load(model_path)

    # Check model structure
    assert hasattr(model, 'forward'), "Model missing forward method"

    # Check weights are not NaN
    for name, param in model.named_parameters():
        if torch.isnan(param).any():
            print(f"NaN detected in {name}")
            sys.exit(1)

    print("Model validation passed")
    sys.exit(0)

except Exception as e:
    print(f"Model validation failed: {e}")
    sys.exit(1)
```

---

## Dynamic Job Generation

### Problem
Number of jobs depends on runtime data.

### Solution: Generate Queue Config Dynamically

```bash
#!/bin/bash
# generate-and-run-queue.sh

# Discover how many files to process
NUM_FILES=$(aws s3 ls s3://data-bucket/input/ | wc -l)

echo "Found $NUM_FILES files, generating queue..."

# Generate queue config
python3 << EOF > /tmp/queue.json
import json

num_files = $NUM_FILES
chunk_size = 100
num_chunks = (num_files + chunk_size - 1) // chunk_size

jobs = [
    {
        "job_id": "download",
        "command": "aws s3 sync s3://data-bucket/input/ /data/input/",
        "timeout": "30m"
    }
]

# Generate processing jobs
for i in range(num_chunks):
    start = i * chunk_size
    end = min((i + 1) * chunk_size, num_files)

    jobs.append({
        "job_id": f"process-chunk-{i}",
        "command": f"python process.py --start {start} --end {end}",
        "depends_on": ["download"],
        "timeout": "2h"
    })

# Aggregate job depends on all processing jobs
aggregate_deps = [f"process-chunk-{i}" for i in range(num_chunks)]
jobs.append({
    "job_id": "aggregate",
    "command": "python aggregate.py /data/processed/ /data/result.csv",
    "depends_on": aggregate_deps,
    "timeout": "30m"
})

queue = {
    "queue_id": "dynamic-queue",
    "jobs": jobs
}

print(json.dumps(queue, indent=2))
EOF

# Launch with generated config
spawn launch --instance-type m7i.xlarge --batch-queue /tmp/queue.json
```

---

## Notifications and Monitoring

### Problem
Want notifications at key pipeline stages.

### Solution: Notification Jobs

```json
{
  "queue_id": "monitored-pipeline",
  "jobs": [
    {
      "job_id": "notify-start",
      "command": "curl -X POST $SLACK_WEBHOOK -d '{\"text\": \"Pipeline started\"}'",
      "timeout": "1m"
    },
    {
      "job_id": "stage1-process",
      "command": "python stage1.py",
      "depends_on": ["notify-start"],
      "timeout": "2h"
    },
    {
      "job_id": "notify-stage1-complete",
      "command": "curl -X POST $SLACK_WEBHOOK -d '{\"text\": \"Stage 1 complete\"}'",
      "depends_on": ["stage1-process"],
      "timeout": "1m"
    },
    {
      "job_id": "stage2-train",
      "command": "python train.py",
      "depends_on": ["notify-stage1-complete"],
      "timeout": "4h"
    },
    {
      "job_id": "notify-complete",
      "command": "curl -X POST $SLACK_WEBHOOK -d '{\"text\": \"Pipeline complete\"}'",
      "depends_on": ["stage2-train"],
      "timeout": "1m"
    }
  ]
}
```

---

## Resource Management

### Problem
Different jobs need different resources (CPU, memory, GPU).

### Solution: Use environment variables to tune resources

```json
{
  "queue_id": "resource-aware-pipeline",
  "jobs": [
    {
      "job_id": "data-processing",
      "command": "python process.py",
      "environment": {
        "NUM_WORKERS": "4",
        "MEMORY_LIMIT": "8GB"
      },
      "timeout": "2h"
    },
    {
      "job_id": "model-training",
      "command": "python train.py",
      "depends_on": ["data-processing"],
      "environment": {
        "CUDA_VISIBLE_DEVICES": "0",
        "TORCH_NUM_THREADS": "8",
        "OMP_NUM_THREADS": "8"
      },
      "timeout": "8h"
    },
    {
      "job_id": "inference",
      "command": "python infer.py",
      "depends_on": ["model-training"],
      "environment": {
        "BATCH_SIZE": "128",
        "NUM_WORKERS": "2"
      },
      "timeout": "1h"
    }
  ]
}
```

---

## See Also

- [Tutorial 5: Batch Queues](../tutorials/05-batch-queues.md) - Queue basics
- [Queue Configuration Reference](../reference/queue-configs.md) - Complete format
- [spawn queue](../reference/commands/queue.md) - Queue command reference
