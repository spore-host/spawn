# Batch Queue Guide

Execute sequential jobs on a single EC2 instance with dependency management, state persistence, and automatic result collection. Optimized for cost efficiency in multi-step workflows.

## Table of Contents

- [Overview](#overview)
- [Quick Start](#quick-start)
- [Queue Templates](#queue-templates)
- [Queue Configuration](#queue-configuration)
- [Job Dependencies](#job-dependencies)
- [Retry Strategies](#retry-strategies)
- [Result Collection](#result-collection)
- [State Management](#state-management)
- [Monitoring](#monitoring)
- [Failure Handling](#failure-handling)
- [Best Practices](#best-practices)
- [Examples](#examples)
- [Troubleshooting](#troubleshooting)

## Overview

Batch queues enable you to:

- **Sequential execution**: Run jobs in dependency order
- **Cost optimization**: Use a single instance instead of multiple parallel instances
- **Automatic retry**: Configurable retry with exponential or fixed backoff
- **State persistence**: Resume from checkpoint after failures
- **Result collection**: Automatically upload outputs to S3
- **Flexible failure handling**: Stop or continue on job failures

**Use Cases:**
- ML pipelines (preprocess → train → evaluate → export)
- Data workflows (extract → transform → load)
- CI/CD pipelines with stages
- Video processing (transcode → thumbnail → upload)
- Sequential testing (build → unit tests → integration tests)

**Cost Comparison:**
```
Parallel sweep (10 instances × 2h): $3.40
Sequential queue (1 instance × 20h): $3.40
Same cost, but queue ensures ordering and dependencies
```

## Quick Start

### Create a Simple Queue

Create `simple-queue.json`:

```json
{
  "queue_id": "my-first-queue",
  "queue_name": "simple-pipeline",
  "jobs": [
    {
      "job_id": "setup",
      "command": "mkdir -p /tmp/results && echo 'Starting...'",
      "timeout": "1m"
    },
    {
      "job_id": "process",
      "command": "for i in {1..10}; do echo \"Item $i\"; sleep 1; done > /tmp/results/output.txt",
      "timeout": "5m",
      "depends_on": ["setup"],
      "result_paths": ["/tmp/results/output.txt"]
    },
    {
      "job_id": "cleanup",
      "command": "echo 'Done!' >> /tmp/results/output.txt",
      "timeout": "1m",
      "depends_on": ["process"],
      "result_paths": ["/tmp/results/output.txt"]
    }
  ],
  "global_timeout": "15m",
  "on_failure": "stop",
  "result_s3_bucket": "spawn-results-us-east-1",
  "result_s3_prefix": "queues/my-first-queue"
}
```

### Launch the Queue

```bash
spawn launch \
  --batch-queue simple-queue.json \
  --instance-type t3.medium \
  --region us-east-1
```

### Monitor Execution

```bash
# Check status
spawn queue status <instance-id>

# Download results
spawn queue results my-first-queue --output ./results/
```

## Queue Templates

Pre-built queue configuration templates for common workflows with variable substitution.

### Available Templates

List all available templates:

```bash
spawn queue template list
```

**Built-in templates:**
- **ml-pipeline** - ML training workflow (preprocess → train → evaluate → export)
- **etl** - ETL pipeline (extract → transform → load → validate)
- **ci-cd** - CI/CD workflow (checkout → build → test → deploy → smoke-test)
- **data-processing** - Data processing (download → process → aggregate → upload)
- **simple-sequential** - Simple 3-step sequential workflow

### View Template Details

Show template structure and variables:

```bash
spawn queue template show ml-pipeline
```

**Output:**
```
Template: ml-pipeline
Description: ML Training Pipeline

Jobs:
  1. preprocess (timeout: 30m)
  2. train (timeout: 2h, depends on: preprocess)
  3. evaluate (timeout: 15m, depends on: train)
  4. export (timeout: 10m, depends on: evaluate)

Required Variables:
  INPUT        - Input data path
  S3_BUCKET    - S3 bucket for results

Optional Variables:
  MODEL        - Model architecture (default: resnet50)
  GPU_DEVICES  - CUDA device IDs (default: 0)
  TRAIN_TIMEOUT - Training timeout (default: 2h)
  ...
```

### Generate from Template

Create a queue configuration from a template:

```bash
# Generate with defaults
spawn queue template generate simple-sequential \
  --var S3_BUCKET=my-results \
  --output queue.json

# Customize ML pipeline
spawn queue template generate ml-pipeline \
  --var INPUT=/data/train.csv \
  --var S3_BUCKET=ml-results \
  --var MODEL=vgg16 \
  --var TRAIN_TIMEOUT=4h \
  --output ml-pipeline.json

# Output to stdout for piping
spawn queue template generate etl \
  --var SOURCE=s3://data/raw \
  --var DESTINATION=postgresql://db \
  --var S3_BUCKET=results | jq .
```

### Template Variables

Templates use `{{VARIABLE}}` syntax for substitution:

**Required variables** - Must be provided via `--var`:
```json
{
  "command": "python train.py --input {{INPUT}}"
}
```

**Optional variables** - Use default if not provided:
```json
{
  "timeout": "{{TRAIN_TIMEOUT:2h}}",
  "env": {
    "CUDA_VISIBLE_DEVICES": "{{GPU_DEVICES:0}}"
  }
}
```

### Launch with Template

**Option 1: Direct launch from template**

Launch directly without generating a file first:

```bash
spawn launch \
  --queue-template ml-pipeline \
  --template-var INPUT=/data/train.csv \
  --template-var S3_BUCKET=ml-results \
  --template-var MODEL=vgg16 \
  --instance-type g5.2xlarge \
  --region us-east-1
```

**Option 2: Generate then launch**

Generate template to file, review, then launch:

```bash
# Generate template to file
spawn queue template generate ml-pipeline \
  --var INPUT=/data/train.csv \
  --var S3_BUCKET=ml-results \
  --output pipeline.json

# Review the generated file
cat pipeline.json

# Launch the queue
spawn launch \
  --batch-queue pipeline.json \
  --instance-type g5.2xlarge \
  --region us-east-1
```

### Template Examples

#### ML Pipeline Template

**Generate config:**
```bash
spawn queue template generate ml-pipeline \
  --var INPUT=/mnt/data/training_set.csv \
  --var S3_BUCKET=my-ml-results \
  --var MODEL=resnet50 \
  --var TRAIN_TIMEOUT=3h \
  --var GPU_DEVICES=0 \
  --output ml-pipeline.json
```

**Or launch directly:**
```bash
spawn launch \
  --queue-template ml-pipeline \
  --template-var INPUT=/mnt/data/training_set.csv \
  --template-var S3_BUCKET=my-ml-results \
  --template-var MODEL=vgg16 \
  --instance-type g5.2xlarge \
  --region us-east-1
```

**Workflow:**
1. **preprocess** - Preprocess input data
2. **train** - Train model with retry logic
3. **evaluate** - Evaluate model performance
4. **export** - Export to ONNX format

**All variables:**
- Required: `INPUT`, `S3_BUCKET`
- Optional: `MODEL`, `GPU_DEVICES`, `TRAIN_TIMEOUT`, `PREPROCESS_TIMEOUT`, `EVAL_TIMEOUT`, `EXPORT_TIMEOUT`, `GLOBAL_TIMEOUT`, `QUEUE_ID`, `S3_PREFIX`, `ON_FAILURE`

#### ETL Pipeline Template

**Generate config:**
```bash
spawn queue template generate etl \
  --var SOURCE=s3://data-lake/raw \
  --var DESTINATION=postgresql://warehouse \
  --var S3_BUCKET=etl-results \
  --output etl-pipeline.json
```

**Or launch directly:**
```bash
spawn launch \
  --queue-template etl \
  --template-var SOURCE=s3://data-lake/raw \
  --template-var DESTINATION=postgresql://warehouse \
  --template-var S3_BUCKET=etl-results \
  --instance-type c5.2xlarge \
  --region us-east-1
```

**Workflow:**
1. **extract** - Extract from source (with retry)
2. **transform** - Transform data
3. **load** - Load to destination (with retry)
4. **validate** - Validate loaded data

**All variables:**
- Required: `SOURCE`, `DESTINATION`, `S3_BUCKET`
- Optional: `EXTRACT_TIMEOUT`, `TRANSFORM_TIMEOUT`, `LOAD_TIMEOUT`, `VALIDATE_TIMEOUT`, `GLOBAL_TIMEOUT`, `QUEUE_ID`, `S3_PREFIX`

#### CI/CD Pipeline Template

**Generate config:**
```bash
spawn queue template generate ci-cd \
  --var REPO_URL=https://github.com/user/repo \
  --var ENDPOINT=https://staging.example.com \
  --var S3_BUCKET=ci-results \
  --var BRANCH=main \
  --var ENVIRONMENT=staging \
  --output ci-cd-pipeline.json
```

**Or launch directly:**
```bash
spawn launch \
  --queue-template ci-cd \
  --template-var REPO_URL=https://github.com/user/repo \
  --template-var ENDPOINT=https://staging.example.com \
  --template-var S3_BUCKET=ci-results \
  --instance-type t3.large \
  --region us-east-1
```

**Workflow:**
1. **checkout** - Clone repository
2. **build** - Build project (with retry)
3. **test** - Run tests
4. **deploy** - Deploy to environment
5. **smoke-test** - Run smoke tests (with retry)

**All variables:**
- Required: `REPO_URL`, `ENDPOINT`, `S3_BUCKET`
- Optional: `BRANCH`, `ENVIRONMENT`, `BUILD_TIMEOUT`, `TEST_TIMEOUT`, `DEPLOY_TIMEOUT`, `GLOBAL_TIMEOUT`, `QUEUE_ID`, `S3_PREFIX`

#### Data Processing Template

**Generate config:**
```bash
spawn queue template generate data-processing \
  --var S3_SOURCE=s3://data/raw \
  --var S3_DESTINATION=s3://data/processed/output.csv \
  --var S3_BUCKET=processing-results \
  --var WORKERS=16 \
  --output data-processing.json
```

**Or launch directly:**
```bash
spawn launch \
  --queue-template data-processing \
  --template-var S3_SOURCE=s3://data/raw \
  --template-var S3_DESTINATION=s3://data/processed/output.csv \
  --template-var S3_BUCKET=processing-results \
  --template-var WORKERS=16 \
  --instance-type c5.4xlarge \
  --region us-east-1
```

**Workflow:**
1. **download** - Download from S3 (with retry)
2. **process** - Process data with configurable workers
3. **aggregate** - Aggregate results
4. **upload** - Upload to S3 (with retry)

**All variables:**
- Required: `S3_SOURCE`, `S3_DESTINATION`, `S3_BUCKET`
- Optional: `WORKERS`, `DOWNLOAD_TIMEOUT`, `PROCESS_TIMEOUT`, `AGGREGATE_TIMEOUT`, `GLOBAL_TIMEOUT`, `QUEUE_ID`, `S3_PREFIX`

#### Simple Sequential Template

**Generate config:**
```bash
spawn queue template generate simple-sequential \
  --var S3_BUCKET=my-results \
  --var STEP1_COMMAND="echo 'Starting...'" \
  --var STEP2_COMMAND="python process.py" \
  --var STEP3_COMMAND="echo 'Done!'" \
  --output simple.json
```

**Or launch directly:**
```bash
spawn launch \
  --queue-template simple-sequential \
  --template-var S3_BUCKET=my-results \
  --instance-type t3.medium \
  --region us-east-1
```

**Workflow:**
1. **step1** - Customizable first step
2. **step2** - Customizable second step (depends on step1)
3. **step3** - Customizable third step (depends on step2)

**All variables:**
- Required: `S3_BUCKET`
- Optional: `STEP1_COMMAND`, `STEP2_COMMAND`, `STEP3_COMMAND`, `STEP1_TIMEOUT`, `STEP2_TIMEOUT`, `STEP3_TIMEOUT`, `GLOBAL_TIMEOUT`, `ON_FAILURE`, `QUEUE_ID`, `S3_PREFIX`

### Benefits of Templates

**Faster setup:**
- No need to write JSON from scratch
- Pre-configured retry logic and timeouts
- Best practices built-in

**Consistency:**
- Standard workflows across projects
- Reduced configuration errors
- Easier to maintain

**Customization:**
- Variable substitution for flexibility
- Override defaults as needed
- Extend templates by editing generated JSON

## Queue Configuration

### Schema Reference

```json
{
  "queue_id": "string",              // Unique identifier (required)
  "queue_name": "string",            // Human-readable name (required)
  "jobs": [                          // Array of jobs (required)
    {
      "job_id": "string",            // Unique job identifier (required)
      "command": "string",           // Shell command to execute (required)
      "timeout": "duration",         // Max execution time (required)
      "depends_on": ["job_id"],      // Job dependencies (optional)
      "env": {                       // Environment variables (optional)
        "KEY": "value"
      },
      "retry": {                     // Retry configuration (optional)
        "max_attempts": 3,
        "backoff": "exponential"     // "exponential" or "fixed"
      },
      "result_paths": ["glob"]       // Files to collect (optional)
    }
  ],
  "global_timeout": "duration",      // Max queue execution time (required)
  "on_failure": "string",            // "stop" or "continue" (required)
  "result_s3_bucket": "string",      // S3 bucket for results (required)
  "result_s3_prefix": "string"       // S3 prefix for results (optional)
}
```

### Field Descriptions

**queue_id**
- Unique identifier for the queue
- Auto-generated if using `spawn launch --batch-queue`
- Format: `queue-YYYYMMDD-HHMMSS`

**queue_name**
- Human-readable name
- Used for logging and identification
- Example: `"ml-training-pipeline"`

**jobs[]**
- Array of job configurations
- Executed in dependency order (topological sort)
- At least one job required

**global_timeout**
- Maximum execution time for entire queue
- Format: `"1h"`, `"30m"`, `"2h30m"`
- Includes all jobs, retries, and backoff delays
- Queue terminates if exceeded

**on_failure**
- `"stop"`: Halt queue on first job failure
- `"continue"`: Skip failed job and proceed to next ready jobs
- Affects all jobs unless they have no dependents

**result_s3_bucket**
- S3 bucket for result storage
- Must exist and be accessible
- Example: `"spawn-results-us-east-1"`

**result_s3_prefix**
- Optional S3 key prefix
- Organizes results by queue
- Example: `"queues/ml-pipeline"`
- Full path: `s3://bucket/prefix/jobs/job-id/file.txt`

### Timeout Format

Use Go duration strings:
- `"30s"` - 30 seconds
- `"5m"` - 5 minutes
- `"2h"` - 2 hours
- `"1h30m"` - 1 hour 30 minutes
- `"24h"` - 24 hours

## Job Dependencies

### Dependency Graph

Jobs execute in topological order based on dependencies.

**Simple Linear:**
```json
{
  "jobs": [
    {"job_id": "step1", "command": "..."},
    {"job_id": "step2", "command": "...", "depends_on": ["step1"]},
    {"job_id": "step3", "command": "...", "depends_on": ["step2"]}
  ]
}
```

Execution order: step1 → step2 → step3

**Parallel Branches:**
```json
{
  "jobs": [
    {"job_id": "start", "command": "..."},
    {"job_id": "branch_a", "command": "...", "depends_on": ["start"]},
    {"job_id": "branch_b", "command": "...", "depends_on": ["start"]},
    {"job_id": "merge", "command": "...", "depends_on": ["branch_a", "branch_b"]}
  ]
}
```

Execution order: start → (branch_a, branch_b) → merge
*Note: branch_a and branch_b run sequentially, not in parallel*

**Diamond Pattern:**
```json
{
  "jobs": [
    {"job_id": "preprocess", "command": "..."},
    {"job_id": "train_model_a", "command": "...", "depends_on": ["preprocess"]},
    {"job_id": "train_model_b", "command": "...", "depends_on": ["preprocess"]},
    {"job_id": "ensemble", "command": "...", "depends_on": ["train_model_a", "train_model_b"]}
  ]
}
```

### Dependency Rules

1. **No circular dependencies**: Queue validation fails if cycles detected
2. **All dependencies must exist**: Reference to non-existent job fails validation
3. **Multiple dependencies**: Job waits for ALL dependencies to complete successfully
4. **No dependencies**: Job executes immediately (in order with other ready jobs)

### Validation

Queue validation happens before launch:

```bash
spawn launch --batch-queue queue.json
# Validates:
# - No duplicate job IDs
# - All dependencies exist
# - No circular dependencies
# - Timeout formats valid
# - Required fields present
```

## Retry Strategies

Configure per-job retry behavior for transient failures.

### Exponential Backoff

**Configuration:**
```json
{
  "job_id": "train",
  "command": "python train.py",
  "timeout": "2h",
  "retry": {
    "max_attempts": 3,
    "backoff": "exponential"
  }
}
```

**Behavior:**
- Attempt 1: Immediate
- Attempt 2: Wait 1s, then retry
- Attempt 3: Wait 2s, then retry
- Attempt 4: Wait 4s, then retry

**Formula:** delay = 2^(attempt-1) seconds

**Use cases:**
- Network errors
- Transient API failures
- Resource temporarily unavailable

### Fixed Backoff

**Configuration:**
```json
{
  "job_id": "api_call",
  "command": "curl https://api.example.com/data",
  "timeout": "1m",
  "retry": {
    "max_attempts": 5,
    "backoff": "fixed"
  }
}
```

**Behavior:**
- Fixed 5-second delay between all attempts

**Use cases:**
- Rate limiting
- Quota exhaustion
- Consistent retry interval needed

### No Retry

Default behavior if retry not specified:

```json
{
  "job_id": "no_retry",
  "command": "python script.py",
  "timeout": "30m"
}
```

**Behavior:** Single attempt only, fails immediately on error.

### Exponential Backoff with Jitter

Adds randomization to prevent simultaneous retries (thundering herd).

**Configuration:**
```json
{
  "job_id": "train",
  "command": "python train.py",
  "timeout": "2h",
  "retry": {
    "max_attempts": 5,
    "backoff": "exponential-jitter",
    "base_delay": "2s",
    "max_delay": "5m",
    "jitter": 0.3
  }
}
```

**Parameters:**
- `base_delay` - Starting delay (default: "5s")
- `max_delay` - Maximum delay cap (default: "5m")
- `jitter` - Randomization factor 0.0-1.0 (0.3 = ±30%)

**Behavior:**
- Attempt 1: Immediate
- Attempt 2: ~2s ± 30% (1.4s - 2.6s)
- Attempt 3: ~4s ± 30% (2.8s - 5.2s)
- Attempt 4: ~8s ± 30% (5.6s - 10.4s)
- Attempt 5: ~16s ± 30% (11.2s - 20.8s)

**Formula:** `delay = min(base * 2^(attempt-1) * (1 + random(-jitter, +jitter)), max_delay)`

**Use cases:**
- Multiple parallel jobs that might fail together
- Distributed systems (prevent coordinated retry storms)
- API rate limiting with multiple workers
- High-concurrency scenarios

### Conditional Retry

Only retry specific exit codes or skip known failure types.

**Configuration:**
```json
{
  "job_id": "download",
  "command": "python download.py",
  "timeout": "30m",
  "retry": {
    "max_attempts": 5,
    "backoff": "exponential",
    "retry_on_codes": [1, 137, 143],
    "dont_retry_on_codes": [2, 127]
  }
}
```

**Parameters:**
- `retry_on_codes` - Whitelist: only retry these exit codes
- `dont_retry_on_codes` - Blacklist: never retry these exit codes (higher priority)

**Common Exit Codes:**
- `0` - Success
- `1` - Generic error (usually retryable)
- `2` - Syntax error / misuse of shell builtin (don't retry)
- `127` - Command not found (don't retry)
- `137` - SIGKILL (interrupted, retryable)
- `143` - SIGTERM (interrupted, retryable)

**Behavior:**
- If `dont_retry_on_codes` specified: Never retry those codes
- If `retry_on_codes` specified: Only retry those codes
- If neither specified: Retry all non-zero exit codes

**Examples:**

Retry only network/interruption errors:
```json
{
  "retry": {
    "max_attempts": 3,
    "retry_on_codes": [1, 137, 143]
  }
}
```

Skip syntax and dependency errors:
```json
{
  "retry": {
    "max_attempts": 5,
    "dont_retry_on_codes": [2, 127]
  }
}
```

**Use cases:**
- Distinguishing transient from permanent failures
- Avoiding retry on config/syntax errors
- Handling interrupted jobs (SIGTERM/SIGKILL)
- Custom error code conventions

### Combined Example: ML Training Pipeline

```json
{
  "job_id": "train",
  "command": "python train.py --data /data --output /tmp/model",
  "timeout": "4h",
  "retry": {
    "max_attempts": 5,
    "backoff": "exponential-jitter",
    "base_delay": "2s",
    "max_delay": "10m",
    "jitter": 0.3,
    "retry_on_codes": [1, 137, 143],
    "dont_retry_on_codes": [2]
  },
  "result_paths": ["/tmp/model/*"]
}
```

**Behavior:**
- Retries up to 5 times
- Uses exponential backoff with ±30% jitter
- Only retries generic errors (1) and interruptions (137, 143)
- Never retries syntax errors (2)
- Caps delay at 10 minutes

### Retry Best Practices

**Do use retry for:**
- ✅ Network operations
- ✅ External API calls
- ✅ Downloading large files
- ✅ Transient GPU errors

**Don't use retry for:**
- ❌ Syntax errors in scripts
- ❌ Missing dependencies
- ❌ Logic errors in code
- ❌ Guaranteed-to-fail operations

**Choosing a Backoff Strategy:**
- Use **exponential-jitter** for distributed/parallel workloads
- Use **exponential** for network/transient errors (single job)
- Use **fixed** for rate limiting/quotas
- Set **max_attempts** based on failure patterns (typically 2-5)

**Using Conditional Retry:**
- Add `retry_on_codes: [1, 137, 143]` for network jobs
- Add `dont_retry_on_codes: [2, 127]` to skip config errors
- Review job logs to identify retryable vs permanent failures
- Use job environment variable `$JOB_ATTEMPT` to debug retry loops

**Jitter Guidelines:**
- Use jitter 0.2-0.3 (20-30%) for most cases
- Higher jitter (0.4-0.5) if many parallel jobs
- No jitter (0.0) for single-job queues
- Jitter prevents synchronized retry storms

## Result Collection

Automatically upload job outputs to S3.

### Configuration

```json
{
  "job_id": "train",
  "command": "python train.py --output /tmp/models",
  "timeout": "2h",
  "result_paths": [
    "/tmp/models/model.pt",
    "/tmp/models/checkpoints/*.ckpt",
    "/logs/training_metrics.json"
  ]
}
```

### Path Types

**Absolute paths:**
```json
"result_paths": ["/tmp/output.txt"]
```

**Glob patterns:**
```json
"result_paths": [
  "/tmp/models/*.pt",      // All .pt files
  "/logs/**/*.log",        // All .log files recursively
  "/data/processed/*.npy"  // All .npy files
]
```

**Multiple patterns:**
```json
"result_paths": [
  "/tmp/model.pt",
  "/tmp/metrics.json",
  "/logs/*.log"
]
```

### Upload Behavior

**Timing:**
- Results uploaded immediately after job completes successfully
- Failed jobs do not upload results (use stdout/stderr logs instead)

**S3 Structure:**
```
s3://{bucket}/{prefix}/jobs/{job_id}/
  ├── model.pt
  ├── metrics.json
  ├── stdout.log
  └── stderr.log
```

**Always included:**
- `stdout.log` - Standard output
- `stderr.log` - Standard error
- Uploaded regardless of result_paths configuration

### Downloading Results

```bash
# Download all results for a queue
spawn queue results queue-20260122-140530 --output ./results/

# Results directory structure:
# results/
#   ├── job1/
#   │   ├── output.txt
#   │   ├── stdout.log
#   │   └── stderr.log
#   ├── job2/
#   │   ├── model.pt
#   │   ├── metrics.json
#   │   ├── stdout.log
#   │   └── stderr.log
#   └── queue-state.json
```

### Result Collection Best Practices

**Do:**
- ✅ Collect only necessary outputs (reduces upload time and cost)
- ✅ Use glob patterns for multiple related files
- ✅ Include metrics/logs for debugging
- ✅ Organize outputs in job-specific directories

**Don't:**
- ❌ Collect entire directories without filters
- ❌ Include temporary files
- ❌ Collect redundant data
- ❌ Use result paths for very large datasets (>10GB per job)

**Size considerations:**
- Result upload is synchronous (blocks before next job)
- Large files increase queue execution time
- Consider using `aws s3 sync` in your job command for large data

## State Management

Queue state persists to `/var/lib/spored/queue-state.json` on the EC2 instance.

### State File Format

```json
{
  "queue_id": "queue-20260122-140530",
  "started_at": "2026-01-22T14:05:30Z",
  "updated_at": "2026-01-22T14:32:15Z",
  "status": "running",
  "jobs": [
    {
      "job_id": "preprocess",
      "status": "completed",
      "started_at": "2026-01-22T14:05:35Z",
      "completed_at": "2026-01-22T14:15:10Z",
      "exit_code": 0,
      "attempt": 1,
      "results_uploaded": true
    },
    {
      "job_id": "train",
      "status": "running",
      "started_at": "2026-01-22T14:15:15Z",
      "attempt": 1,
      "pid": 12345
    },
    {
      "job_id": "evaluate",
      "status": "pending",
      "attempt": 0
    }
  ]
}
```

### Job Status Values

- `"pending"` - Not yet started
- `"running"` - Currently executing
- `"completed"` - Finished successfully (exit code 0)
- `"failed"` - Finished with error (non-zero exit code)
- `"skipped"` - Skipped due to dependency failure and `on_failure: "continue"`

### Resume Capability

Queue automatically resumes from state file:

**Scenario 1: Instance interruption**
- Spot instance terminated mid-execution
- On restart, queue loads state
- Skips completed jobs
- Retries failed jobs (within retry limits)
- Continues from next pending job

**Scenario 2: Agent crash**
- spored process terminated
- On restart, queue loads state
- Resumes from checkpoint

**Note:** Resume only works if instance restarts (not for instance termination).

### State Update Frequency

State updates atomically after each event:
- Job starts: Update status to "running", set PID
- Job completes: Update status, exit code, timestamps
- Results uploaded: Set results_uploaded flag

**Atomic writes:** Uses temp file + rename to prevent corruption.

## Monitoring

### Real-Time Status

Check queue execution status:

```bash
spawn queue status <instance-id>
```

**Output:**
```
📊 Queue Status
   Instance: i-1234567890abcdef0

Getting instance details...
Connecting to 54.123.45.67...

Queue ID:    queue-20260122-140530
Status:      running
Started:     2026-01-22T14:05:30Z
Updated:     2026-01-22T14:32:15Z

Jobs:
JOB ID                    STATUS      ATTEMPT  EXIT     RESULTS
--------------------------------------------------------------------------------
preprocess                completed   1        0        yes
train                     running     1        -        no       (PID: 12345)
evaluate                  pending     0        -        no
export                    pending     0        -        no
```

### Monitoring Logs

**Job logs:**
```bash
# SSH to instance
ssh -i key.pem ec2-user@<instance-ip>

# View job stdout
tail -f /var/log/spored/jobs/train-stdout.log

# View job stderr
tail -f /var/log/spored/jobs/train-stderr.log

# View queue runner logs
sudo journalctl -u spored -f
```

**CloudWatch Logs:**
```bash
# If CloudWatch agent is configured
aws logs tail /aws/ec2/spored --follow
```

### Completion Notification

Queue completes when:
1. All jobs finish successfully, OR
2. A job fails and `on_failure: "stop"`, OR
3. Global timeout exceeded

**Final state uploaded to S3:**
```
s3://{bucket}/{prefix}/queue-state.json
```

## Failure Handling

### Failure Modes

**`on_failure: "stop"`** (recommended for critical pipelines)

Behavior:
- First job failure halts entire queue
- Subsequent jobs marked as "skipped"
- Instance terminates after cleanup
- State file saved with failure info

Example:
```json
{
  "on_failure": "stop",
  "jobs": [
    {"job_id": "validate", "command": "..."},
    {"job_id": "process", "command": "...", "depends_on": ["validate"]},
    {"job_id": "export", "command": "...", "depends_on": ["process"]}
  ]
}
```

If validate fails: process and export are skipped.

**`on_failure: "continue"`** (for independent steps)

Behavior:
- Job failure is recorded
- Queue continues to next ready job
- Jobs depending on failed job are skipped
- Jobs without dependency on failed job still execute

Example:
```json
{
  "on_failure": "continue",
  "jobs": [
    {"job_id": "model_a", "command": "..."},
    {"job_id": "model_b", "command": "..."},
    {"job_id": "ensemble", "command": "...", "depends_on": ["model_a", "model_b"]}
  ]
}
```

If model_a fails: model_b still runs, ensemble is skipped.

### Debugging Failures

**1. Check queue status:**
```bash
spawn queue status <instance-id>
```

**2. Review state file:**
```bash
spawn queue results <queue-id> --output ./results/
cat results/queue-state.json
```

**3. Check job logs:**
```bash
# After downloading results
cat results/failed-job/stderr.log
cat results/failed-job/stdout.log
```

**4. Common failure causes:**
- Command syntax errors
- Missing dependencies/packages
- File not found
- Insufficient disk space
- Timeout exceeded
- Out of memory

**5. Fix and retry:**
```bash
# Fix command in queue.json
# Relaunch queue
spawn launch --batch-queue fixed-queue.json
```

### Timeout Handling

**Job timeout:**
```json
{
  "job_id": "train",
  "command": "python train.py",
  "timeout": "2h"  // Job terminated after 2 hours
}
```

**Behavior:**
- Process receives SIGTERM
- 30-second grace period for cleanup
- Then SIGKILL
- Job marked as failed
- Retry logic applies (if configured)

**Global timeout:**
```json
{
  "global_timeout": "8h"  // Entire queue terminated after 8 hours
}
```

**Behavior:**
- All running jobs receive SIGTERM
- Queue status set to "failed"
- State saved with timeout error
- Instance terminates

### Error Messages

Check `error_message` field in state for details:

```json
{
  "job_id": "train",
  "status": "failed",
  "exit_code": 1,
  "error_message": "command execution failed: exit status 1",
  "attempt": 3
}
```

## Best Practices

### Job Design

**Keep jobs focused:**
```json
// Good: Single responsibility
{"job_id": "train", "command": "python train.py"}
{"job_id": "evaluate", "command": "python eval.py"}

// Bad: Multiple responsibilities
{"job_id": "train_and_eval", "command": "python train.py && python eval.py"}
```

**Use dependencies for ordering:**
```json
// Good: Explicit dependencies
{"job_id": "preprocess", "command": "..."},
{"job_id": "train", "command": "...", "depends_on": ["preprocess"]}

// Bad: Implicit ordering in command
{"job_id": "all", "command": "preprocess.sh && train.sh"}
```

**Set appropriate timeouts:**
```json
// Good: Realistic timeouts
{"job_id": "quick_test", "timeout": "5m"}
{"job_id": "training", "timeout": "4h"}

// Bad: Too generous
{"job_id": "quick_test", "timeout": "24h"}
```

### Environment Variables

Use `env` for job-specific configuration:

```json
{
  "job_id": "train",
  "command": "python train.py",
  "timeout": "2h",
  "env": {
    "CUDA_VISIBLE_DEVICES": "0",
    "BATCH_SIZE": "32",
    "LEARNING_RATE": "0.001",
    "DATA_DIR": "/mnt/data",
    "OUTPUT_DIR": "/tmp/models"
  }
}
```

**Automatically available:**
- `JOB_ID` - Current job ID
- `JOB_ATTEMPT` - Current attempt number (1, 2, 3, ...)
- `QUEUE_ID` - Queue identifier

**Access in scripts:**
```bash
#!/bin/bash
echo "Running job: $JOB_ID"
echo "Attempt: $JOB_ATTEMPT"
python train.py --output "/tmp/results/$JOB_ID"
```

### Resource Management

**Disk space:**
```json
{
  "job_id": "cleanup_after_preprocess",
  "command": "rm -rf /tmp/large_intermediate_files",
  "timeout": "5m",
  "depends_on": ["train"]
}
```

**Memory:**
- Monitor instance memory usage
- Choose instance type with adequate RAM
- Use disk-backed operations for large datasets

**GPU:**
```json
{
  "job_id": "train_model_a",
  "command": "python train.py --model a",
  "env": {"CUDA_VISIBLE_DEVICES": "0"}
},
{
  "job_id": "train_model_b",
  "command": "python train.py --model b",
  "env": {"CUDA_VISIBLE_DEVICES": "1"}
}
```

### Cost Optimization

**Choose right instance type:**
```bash
# CPU-bound pipeline
--instance-type c5.4xlarge

# Memory-bound pipeline
--instance-type r5.2xlarge

# GPU pipeline
--instance-type g5.2xlarge

# General purpose
--instance-type t3.xlarge
```

**Use Spot instances carefully:**
```bash
# Good for short pipelines (<2h)
spawn launch --batch-queue queue.json --spot

# Avoid for long pipelines (>4h)
# Risk of interruption increases with duration
```

**Optimize job order:**
```json
// Good: Fast jobs first for quicker failure detection
{
  "jobs": [
    {"job_id": "validate_inputs", "timeout": "1m"},
    {"job_id": "expensive_training", "timeout": "4h", "depends_on": ["validate_inputs"]}
  ]
}
```

## Examples

### Example 1: ML Training Pipeline

Complete pipeline from data preparation to model export.

**`ml-pipeline.json`:**
```json
{
  "queue_id": "ml-pipeline",
  "queue_name": "end-to-end-ml-pipeline",
  "jobs": [
    {
      "job_id": "preprocess",
      "command": "python preprocess.py --input /data/raw --output /data/processed",
      "timeout": "30m",
      "env": {
        "NUM_WORKERS": "4"
      },
      "retry": {
        "max_attempts": 3,
        "backoff": "exponential"
      },
      "result_paths": [
        "/data/processed/metadata.json"
      ]
    },
    {
      "job_id": "train",
      "command": "python train.py --data /data/processed --output /models --epochs 100",
      "timeout": "2h",
      "depends_on": ["preprocess"],
      "env": {
        "CUDA_VISIBLE_DEVICES": "0",
        "BATCH_SIZE": "32"
      },
      "retry": {
        "max_attempts": 2,
        "backoff": "fixed"
      },
      "result_paths": [
        "/models/model.pt",
        "/models/training_history.json"
      ]
    },
    {
      "job_id": "evaluate",
      "command": "python evaluate.py --model /models/model.pt --test /data/test --output /results",
      "timeout": "15m",
      "depends_on": ["train"],
      "env": {
        "CUDA_VISIBLE_DEVICES": "0"
      },
      "result_paths": [
        "/results/eval_metrics.json",
        "/results/confusion_matrix.png"
      ]
    },
    {
      "job_id": "export",
      "command": "python export_model.py --model /models/model.pt --format onnx --output /export",
      "timeout": "10m",
      "depends_on": ["evaluate"],
      "result_paths": [
        "/export/model.onnx"
      ]
    }
  ],
  "global_timeout": "4h",
  "on_failure": "stop",
  "result_s3_bucket": "spawn-results-us-east-1",
  "result_s3_prefix": "queues/ml-pipeline"
}
```

**Launch:**
```bash
spawn launch \
  --batch-queue ml-pipeline.json \
  --instance-type g5.2xlarge \
  --region us-east-1
```

### Example 2: Data Processing ETL

Extract, transform, load pipeline with validation.

**`etl-pipeline.json`:**
```json
{
  "queue_id": "etl-pipeline",
  "queue_name": "daily-etl",
  "jobs": [
    {
      "job_id": "extract",
      "command": "python extract.py --source s3://data-lake/raw/ --output /tmp/extracted/",
      "timeout": "20m",
      "retry": {
        "max_attempts": 5,
        "backoff": "fixed"
      }
    },
    {
      "job_id": "transform",
      "command": "python transform.py --input /tmp/extracted/ --output /tmp/transformed/",
      "timeout": "1h",
      "depends_on": ["extract"],
      "result_paths": [
        "/tmp/transformed/summary.json"
      ]
    },
    {
      "job_id": "validate",
      "command": "python validate.py --data /tmp/transformed/",
      "timeout": "10m",
      "depends_on": ["transform"]
    },
    {
      "job_id": "load",
      "command": "python load.py --input /tmp/transformed/ --dest s3://data-lake/processed/",
      "timeout": "30m",
      "depends_on": ["validate"],
      "retry": {
        "max_attempts": 3,
        "backoff": "exponential"
      }
    }
  ],
  "global_timeout": "3h",
  "on_failure": "stop",
  "result_s3_bucket": "spawn-results-us-east-1",
  "result_s3_prefix": "queues/etl-pipeline"
}
```

### Example 3: Parallel Model Training with Ensemble

Train multiple models and combine them.

**`ensemble-pipeline.json`:**
```json
{
  "queue_id": "ensemble",
  "queue_name": "ensemble-training",
  "jobs": [
    {
      "job_id": "preprocess",
      "command": "python preprocess.py",
      "timeout": "20m"
    },
    {
      "job_id": "train_resnet",
      "command": "python train.py --model resnet50",
      "timeout": "2h",
      "depends_on": ["preprocess"],
      "result_paths": ["/models/resnet50.pt"]
    },
    {
      "job_id": "train_efficientnet",
      "command": "python train.py --model efficientnet",
      "timeout": "2h",
      "depends_on": ["preprocess"],
      "result_paths": ["/models/efficientnet.pt"]
    },
    {
      "job_id": "train_vit",
      "command": "python train.py --model vit",
      "timeout": "2h",
      "depends_on": ["preprocess"],
      "result_paths": ["/models/vit.pt"]
    },
    {
      "job_id": "ensemble",
      "command": "python ensemble.py --models /models/*.pt --output /models/ensemble.pt",
      "timeout": "30m",
      "depends_on": ["train_resnet", "train_efficientnet", "train_vit"],
      "result_paths": [
        "/models/ensemble.pt",
        "/results/ensemble_metrics.json"
      ]
    }
  ],
  "global_timeout": "8h",
  "on_failure": "continue",
  "result_s3_bucket": "spawn-results-us-east-1",
  "result_s3_prefix": "queues/ensemble"
}
```

*Note: Models train sequentially, not in parallel. If one fails, others still run.*

## Troubleshooting

### Queue Won't Start

**Symptom:** `spawn launch --batch-queue` fails immediately.

**Check:**
1. Queue validation errors:
   ```bash
   # Look for validation error messages
   spawn launch --batch-queue queue.json
   ```

2. Common issues:
   - Duplicate job IDs
   - Circular dependencies
   - Invalid timeout format
   - Missing required fields

**Resolution:** Fix validation errors in queue.json.

### Job Stuck in "Running"

**Symptom:** Job shows "running" status for longer than expected.

**Check:**
1. SSH to instance:
   ```bash
   ssh -i key.pem ec2-user@<instance-ip>
   ```

2. Check process:
   ```bash
   ps aux | grep <PID>
   ```

3. Check logs:
   ```bash
   tail -f /var/log/spored/jobs/<job-id>-stdout.log
   tail -f /var/log/spored/jobs/<job-id>-stderr.log
   ```

**Common causes:**
- Job is legitimately long-running
- Job hung waiting for input
- Deadlock or infinite loop

**Resolution:**
- Wait for timeout
- Or manually terminate instance if truly stuck

### Job Fails Immediately

**Symptom:** Job fails with exit code immediately after starting.

**Check:**
1. Download results:
   ```bash
   spawn queue results <queue-id> --output ./results/
   ```

2. Read stderr:
   ```bash
   cat results/<job-id>/stderr.log
   ```

**Common causes:**
- Command not found
- Syntax error in command
- Missing dependencies
- File not found
- Permission denied

**Resolution:** Fix command in queue.json and relaunch.

### Results Not Uploading

**Symptom:** `results_uploaded: false` in state file.

**Check:**
1. S3 bucket exists and is accessible
2. Instance has S3 write permissions (IAM role)
3. Result paths exist:
   ```bash
   # SSH to instance
   ls -la /path/to/results/
   ```

**Common causes:**
- Result files not created by job
- Wrong path in result_paths
- No IAM permissions for S3
- S3 bucket doesn't exist

**Resolution:**
- Fix result_paths in queue.json
- Ensure job creates output files
- Add S3 write permissions to instance IAM role

### Instance Terminated Unexpectedly

**Symptom:** Queue stops mid-execution, instance terminated.

**Check:**
1. Spot interruption (if using --spot):
   ```bash
   # Check CloudWatch or instance metadata
   ```

2. Global timeout exceeded:
   ```bash
   # Check queue-state.json
   cat results/queue-state.json
   ```

3. Out of disk space:
   ```bash
   # SSH and check
   df -h
   ```

**Resolution:**
- For spot: Relaunch without --spot
- For timeout: Increase global_timeout
- For disk: Use larger disk or clean up between jobs

### Can't SSH to Instance

**Symptom:** SSH connection refused or timeout.

**Check:**
1. Security group allows SSH (port 22)
2. Instance has public IP
3. Correct key file and permissions:
   ```bash
   chmod 400 key.pem
   ```

**Note:** Queue monitoring via `spawn queue status` doesn't require SSH.

## Advanced Topics

### Custom Instance Configuration

Pass additional launch options:

```bash
spawn launch \
  --batch-queue queue.json \
  --instance-type g5.2xlarge \
  --disk-size 500 \
  --ami ami-custom-image \
  --security-group sg-12345 \
  --subnet subnet-12345 \
  --iam-role spawn-queue-role
```

### Pre-installing Dependencies

Use custom AMI with dependencies pre-installed:

```bash
# Create custom AMI
# 1. Launch base instance
# 2. Install dependencies
# 3. Create AMI
# 4. Use in queue launch

spawn launch --batch-queue queue.json --ami ami-custom
```

### Long-Running Queues

For queues >24 hours:

1. Use on-demand instances (not spot)
2. Enable CloudWatch detailed monitoring
3. Set up alarms for failures
4. Use larger global_timeout
5. Implement checkpointing in jobs

### Integration with Scheduled Executions

Combine with scheduled sweeps:

```bash
# Schedule daily queue execution
spawn schedule create queue-params.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York"
```

Where `queue-params.yaml` launches a batch queue instead of parallel sweep.

## See Also

- [Scheduled Executions Guide](SCHEDULED_EXECUTIONS_GUIDE.md)
- [Parameter Sweep Guide](PARAMETER_SWEEP_GUIDE.md)
- [Examples](examples/README.md)
- [Troubleshooting](TROUBLESHOOTING.md)
