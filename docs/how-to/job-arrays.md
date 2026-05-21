# How-To: Job Arrays

Advanced patterns for parallel job arrays.

## Chunked Processing

### Problem
Process 10,000 files using 100 instances (100 files per instance).

### Solution: Chunked Arrays

```bash
#!/bin/bash
# process-chunk.sh

CHUNK_SIZE=100
START=$((TASK_ARRAY_INDEX * CHUNK_SIZE))
END=$((START + CHUNK_SIZE))

echo "Processing files $START to $((END - 1))"

for i in $(seq $START $((END - 1))); do
  FILE_NUM=$(printf "%05d" $i)
  INPUT_FILE="s3://my-bucket/input/file-${FILE_NUM}.csv"
  OUTPUT_FILE="s3://my-bucket/output/file-${FILE_NUM}-processed.csv"

  echo "Processing file $FILE_NUM..."
  aws s3 cp "$INPUT_FILE" /tmp/input.csv
  python process.py /tmp/input.csv /tmp/output.csv
  aws s3 cp /tmp/output.csv "$OUTPUT_FILE"
  rm /tmp/input.csv /tmp/output.csv
done

echo "Completed chunk $TASK_ARRAY_INDEX"
spored complete --status success
```

**Launch:**
```bash
spawn launch \
  --instance-type c7i.large \
  --array 100 \
  --ttl 4h \
  --iam-policy s3:FullAccess \
  --on-complete terminate \
  --user-data @process-chunk.sh
```

**Result:**
- 100 instances
- Each processes 100 files
- Total: 10,000 files processed

---

## Dynamic Task Distribution

### Problem
Tasks have variable duration. Fixed chunking wastes resources.

### Solution: SQS Queue Pattern

**Setup:**
```bash
# Create SQS queue
QUEUE_URL=$(aws sqs create-queue --queue-name spawn-tasks --query 'QueueUrl' --output text)

# Populate queue with tasks
for i in {1..10000}; do
  aws sqs send-message --queue-url $QUEUE_URL \
    --message-body "file-$(printf "%05d" $i).csv"
done

echo "Queue populated with 10,000 tasks"
```

**Worker script:**
```bash
#!/bin/bash
# worker.sh
set -e

QUEUE_URL="https://sqs.us-east-1.amazonaws.com/123456789012/spawn-tasks"
PROCESSED=0

echo "Worker $TASK_ARRAY_INDEX starting..."

while true; do
  # Get task from queue
  MESSAGE=$(aws sqs receive-message \
    --queue-url $QUEUE_URL \
    --max-number-of-messages 1 \
    --visibility-timeout 300 \
    --wait-time-seconds 20 \
    --query 'Messages[0]' \
    --output json)

  if [ "$MESSAGE" == "null" ]; then
    echo "Queue empty, worker done"
    break
  fi

  # Extract task and receipt handle
  TASK=$(echo $MESSAGE | jq -r '.Body')
  RECEIPT=$(echo $MESSAGE | jq -r '.ReceiptHandle')

  echo "Processing: $TASK"

  # Process task
  aws s3 cp "s3://my-bucket/input/$TASK" /tmp/input.csv
  python process.py /tmp/input.csv /tmp/output.csv
  aws s3 cp /tmp/output.csv "s3://my-bucket/output/$TASK"

  # Delete message from queue
  aws sqs delete-message --queue-url $QUEUE_URL --receipt-handle "$RECEIPT"

  PROCESSED=$((PROCESSED + 1))
  echo "Completed $PROCESSED tasks"
done

echo "Worker $TASK_ARRAY_INDEX finished: $PROCESSED tasks"
spored complete --status success
```

**Launch:**
```bash
spawn launch \
  --instance-type c7i.large \
  --array 50 \
  --ttl 8h \
  --iam-policy sqs:FullAccess,s3:FullAccess \
  --on-complete terminate \
  --user-data @worker.sh
```

**Benefits:**
- Workers pull tasks dynamically
- Faster workers do more work
- No wasted capacity
- Handles variable task durations
- Easy to add more workers if needed

---

## Fault Tolerance with Retry

### Problem
Some tasks fail transiently. Want automatic retry.

### Solution: Retry Logic in Worker

```bash
#!/bin/bash
# worker-with-retry.sh

process_task() {
  local task=$1
  local max_attempts=3
  local attempt=1

  while [ $attempt -le $max_attempts ]; do
    echo "Processing $task (attempt $attempt/$max_attempts)"

    if process_file "$task"; then
      echo "Success on attempt $attempt"
      return 0
    else
      echo "Failed attempt $attempt"
      if [ $attempt -lt $max_attempts ]; then
        sleep $((2 ** attempt))  # Exponential backoff
      fi
      attempt=$((attempt + 1))
    fi
  done

  echo "Failed after $max_attempts attempts"
  return 1
}

process_file() {
  local task=$1

  # Download
  aws s3 cp "s3://bucket/input/$task" /tmp/input.csv || return 1

  # Process
  python process.py /tmp/input.csv /tmp/output.csv || return 1

  # Upload
  aws s3 cp /tmp/output.csv "s3://bucket/output/$task" || return 1

  return 0
}

# Main loop
while true; do
  MESSAGE=$(aws sqs receive-message --queue-url $QUEUE_URL ...)
  [ "$MESSAGE" == "null" ] && break

  TASK=$(echo $MESSAGE | jq -r '.Body')
  RECEIPT=$(echo $MESSAGE | jq -r '.ReceiptHandle')

  if process_task "$TASK"; then
    # Success: delete from queue
    aws sqs delete-message --queue-url $QUEUE_URL --receipt-handle "$RECEIPT"
  else
    # Failed: leave in queue (will be retried by another worker)
    echo "Task failed, leaving in queue for retry"
  fi
done
```

---

## Progress Tracking

### Problem
Want to monitor progress across 1000 workers.

### Solution: DynamoDB Progress Table

**Setup:**
```bash
# Create DynamoDB table for progress
aws dynamodb create-table \
  --table-name spawn-progress \
  --attribute-definitions \
    AttributeName=array_id,AttributeType=S \
    AttributeName=task_index,AttributeType=N \
  --key-schema \
    AttributeName=array_id,KeyType=HASH \
    AttributeName=task_index,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST
```

**Worker with progress tracking:**
```bash
#!/bin/bash
# worker-tracked.sh

ARRAY_ID="array-20260127-abc123"
PROGRESS_TABLE="spawn-progress"

update_progress() {
  local status=$1
  local tasks_completed=$2

  aws dynamodb put-item \
    --table-name $PROGRESS_TABLE \
    --item "{
      \"array_id\": {\"S\": \"$ARRAY_ID\"},
      \"task_index\": {\"N\": \"$TASK_ARRAY_INDEX\"},
      \"status\": {\"S\": \"$status\"},
      \"tasks_completed\": {\"N\": \"$tasks_completed\"},
      \"last_update\": {\"S\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"},
      \"hostname\": {\"S\": \"$(hostname)\"}
    }"
}

# Update: started
update_progress "running" 0

# Process tasks
COMPLETED=0
while true; do
  # ... process task ...
  COMPLETED=$((COMPLETED + 1))

  # Update progress every 10 tasks
  if [ $((COMPLETED % 10)) -eq 0 ]; then
    update_progress "running" $COMPLETED
  fi
done

# Update: completed
update_progress "completed" $COMPLETED
spored complete --status success
```

**Monitor progress:**
```bash
#!/bin/bash
# monitor-progress.sh

ARRAY_ID="array-20260127-abc123"
PROGRESS_TABLE="spawn-progress"

while true; do
  # Query all workers
  ITEMS=$(aws dynamodb scan \
    --table-name $PROGRESS_TABLE \
    --filter-expression "array_id = :array_id" \
    --expression-attribute-values '{":array_id":{"S":"'$ARRAY_ID'"}}' \
    --query 'Items[*].[task_index.N, status.S, tasks_completed.N]' \
    --output text)

  # Count by status
  RUNNING=$(echo "$ITEMS" | grep running | wc -l)
  COMPLETED=$(echo "$ITEMS" | grep completed | wc -l)
  TOTAL_TASKS=$(echo "$ITEMS" | awk '{sum += $3} END {print sum}')

  clear
  echo "Array Progress: $ARRAY_ID"
  echo "================================"
  echo "Running workers: $RUNNING"
  echo "Completed workers: $COMPLETED"
  echo "Total tasks processed: $TOTAL_TASKS"
  echo ""
  echo "Recent updates:"
  echo "$ITEMS" | sort -n | tail -10

  sleep 10
done
```

---

## Spot Instance Array with Checkpoint

### Problem
Using spot instances for array, need to handle interruptions.

### Solution: Checkpoint Progress

```bash
#!/bin/bash
# spot-worker.sh

STATE_FILE="/tmp/worker-state.json"
CHECKPOINT_S3="s3://my-bucket/checkpoints/worker-${TASK_ARRAY_INDEX}.json"

# Load checkpoint if exists
if aws s3 cp "$CHECKPOINT_S3" "$STATE_FILE" 2>/dev/null; then
  RESUME_FROM=$(jq -r '.last_completed' $STATE_FILE)
  echo "Resuming from task $RESUME_FROM"
else
  RESUME_FROM=0
  echo "Starting fresh"
fi

# Checkpoint function
checkpoint() {
  echo "{\"last_completed\": $1, \"timestamp\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" > $STATE_FILE
  aws s3 cp $STATE_FILE "$CHECKPOINT_S3"
}

# Trap spot interruption signal
trap 'echo "Spot interruption! Checkpointing..."; checkpoint $COMPLETED; exit 0' SIGTERM

# Process tasks
COMPLETED=$RESUME_FROM
CHUNK_SIZE=100
START=$((TASK_ARRAY_INDEX * CHUNK_SIZE))
END=$((START + CHUNK_SIZE))

for i in $(seq $START $((END - 1))); do
  if [ $i -lt $RESUME_FROM ]; then
    continue  # Skip already completed
  fi

  # Process task
  process_file $i

  COMPLETED=$i

  # Checkpoint every 10 tasks
  if [ $((i % 10)) -eq 0 ]; then
    checkpoint $COMPLETED
  fi
done

# Final checkpoint
checkpoint $COMPLETED
spored complete --status success
```

**If interrupted:**
- Checkpoint saved to S3
- Relaunch with same user data
- Worker resumes from last checkpoint

---

## Conditional Processing

### Problem
Only process files that don't already have results.

### Solution: Check Before Processing

```bash
#!/bin/bash
# conditional-worker.sh

process_if_needed() {
  local input_file=$1
  local output_file=$2

  # Check if output already exists
  if aws s3 ls "$output_file" &> /dev/null; then
    echo "Output exists, skipping: $output_file"
    return 0
  fi

  echo "Processing: $input_file"

  # Download
  aws s3 cp "$input_file" /tmp/input.csv

  # Process
  python process.py /tmp/input.csv /tmp/output.csv

  # Upload
  aws s3 cp /tmp/output.csv "$output_file"

  echo "Completed: $output_file"
}

# Process chunk
CHUNK_SIZE=100
START=$((TASK_ARRAY_INDEX * CHUNK_SIZE))
END=$((START + CHUNK_SIZE))

PROCESSED=0
SKIPPED=0

for i in $(seq $START $((END - 1))); do
  FILE_NUM=$(printf "%05d" $i)
  INPUT="s3://my-bucket/input/file-${FILE_NUM}.csv"
  OUTPUT="s3://my-bucket/output/file-${FILE_NUM}-result.csv"

  if process_if_needed "$INPUT" "$OUTPUT"; then
    if aws s3 ls "$OUTPUT" &> /dev/null; then
      SKIPPED=$((SKIPPED + 1))
    else
      PROCESSED=$((PROCESSED + 1))
    fi
  fi
done

echo "Worker $TASK_ARRAY_INDEX: Processed $PROCESSED, Skipped $SKIPPED"
spored complete --status success
```

**Use case:**
- Resuming partially completed sweep
- Incremental processing
- Handling failures (reprocess only failed files)

---

## Resource-Aware Scheduling

### Problem
Some tasks need more memory/CPU than others.

### Solution: Multiple Arrays with Different Instance Types

```yaml
# small-tasks.yaml
defaults:
  instance_type: t3.medium
  ttl: 2h
  spot: true

params:
  - name: small-001
    task_type: small
  - name: small-002
    task_type: small
  # ... 100 small tasks
```

```yaml
# large-tasks.yaml
defaults:
  instance_type: m7i.2xlarge
  ttl: 4h
  spot: true

params:
  - name: large-001
    task_type: large
  - name: large-002
    task_type: large
  # ... 20 large tasks
```

**Launch:**
```bash
# Launch small tasks array
spawn launch --param-file small-tasks.yaml

# Launch large tasks array
spawn launch --param-file large-tasks.yaml
```

---

## Aggregating Results

### Problem
1000 workers produce results. Need to aggregate.

### Solution: Hierarchical Aggregation

**Workers output to S3:**
```bash
# Each worker outputs: s3://bucket/results/worker-${TASK_ARRAY_INDEX}.json
aws s3 cp /tmp/result.json "s3://bucket/results/worker-${TASK_ARRAY_INDEX}.json"
```

**Aggregation job after array completes:**
```bash
#!/bin/bash
# aggregate-results.sh

echo "Aggregating results from 1000 workers..."

# Download all results
aws s3 sync s3://bucket/results/ /tmp/results/

# Aggregate with Python
python3 << 'EOF'
import json
import glob

results = []
for file in glob.glob('/tmp/results/worker-*.json'):
    with open(file) as f:
        results.append(json.load(f))

# Aggregate
total = sum(r['processed'] for r in results)
errors = sum(r['errors'] for r in results)
duration = max(r['duration'] for r in results)

summary = {
    'total_processed': total,
    'total_errors': errors,
    'max_duration': duration,
    'workers': len(results)
}

print(json.dumps(summary, indent=2))

with open('/tmp/summary.json', 'w') as f:
    json.dump(summary, f, indent=2)
EOF

# Upload summary
aws s3 cp /tmp/summary.json s3://bucket/summary.json

echo "Aggregation complete"
```

**Launch aggregation after array:**
```bash
# Wait for array to complete
spawn status array-xxx --wait

# Launch aggregation job
spawn launch --instance-type t3.medium --ttl 30m \
  --iam-policy s3:FullAccess \
  --on-complete terminate \
  --user-data @aggregate-results.sh
```

---

## Cost Optimization

### Strategy 1: Right-Size Based on Profiling

```bash
# Profile task on different instance types
for TYPE in t3.medium m7i.large c7i.xlarge; do
  echo "Testing $TYPE..."
  spawn launch --instance-type $TYPE --ttl 30m --user-data @benchmark.sh
done

# Results:
# t3.medium: 60s per task, $0.0416/h = $0.000693/task
# m7i.large: 30s per task, $0.1008/h = $0.000840/task
# c7i.xlarge: 20s per task, $0.1701/h = $0.000945/task

# Winner: m7i.large (best speed/cost ratio)
```

### Strategy 2: Batch Size Optimization

```bash
# Test different chunk sizes
# Small chunks: More overhead, less wasted if interrupted
# Large chunks: Less overhead, more wasted if interrupted

# For 10,000 tasks:
# 1000 instances × 10 tasks each = High overhead
# 100 instances × 100 tasks each = Good balance ✓
# 10 instances × 1000 tasks each = Low overhead but slow
```

---

## See Also

- [Tutorial 4: Job Arrays](../tutorials/04-job-arrays.md) - Job array basics
- [How-To: Spot Instances](spot-instances.md) - Spot for arrays
- [How-To: Cost Optimization](cost-optimization.md) - Cost strategies
- [spawn launch](../reference/commands/launch.md) - Array flags
