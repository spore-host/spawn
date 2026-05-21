# Tutorial 4: Job Arrays

**Duration:** 30 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md)

## What You'll Learn

In this tutorial, you'll learn how to launch identical instances in parallel:
- Launch job arrays (100s of identical instances)
- Use array indices for task distribution
- Process data files in parallel
- Implement worker pool patterns
- Optimize costs for batch workloads

## Job Arrays vs Parameter Sweeps

| Feature | Job Arrays | Parameter Sweeps |
|---------|-----------|------------------|
| **Configuration** | Identical instances | Different parameters per instance |
| **Use Case** | Process N files/tasks | Hyperparameter tuning, grid search |
| **Indexing** | Array index (0-99) | Named parameters |
| **Syntax** | `--array 100` | `--param-file sweep.yaml` |
| **Scale** | 100s-1000s | Typically 10s-100s |

**When to use job arrays:**
- Process 100 data files
- Render 500 video frames
- Run Monte Carlo simulations
- Embarrassingly parallel workloads

**When to use parameter sweeps:**
- ML hyperparameter search
- Different configurations per run
- Named experiments

## Understanding Job Arrays

A job array consists of:

1. **Array Size** - Number of identical instances (e.g., 100)
2. **Array Index** - Unique index per instance (0-99)
3. **Task Mapping** - Use index to determine what work to do

**Key Concept:** Each instance gets unique `TASK_ARRAY_INDEX` environment variable.

**Example:**
```bash
spawn launch --array 100
```
= 100 identical instances, indexed 0-99

## Your First Job Array

Let's start with a simple 5-instance array.

### Step 1: Launch Array

```bash
spawn launch \
  --instance-type t3.micro \
  --array 5 \
  --ttl 30m \
  --name my-array \
  --user-data '#!/bin/bash
echo "Task index: $TASK_ARRAY_INDEX"
echo "Total tasks: $TASK_ARRAY_SIZE"
echo "Result: $((TASK_ARRAY_INDEX * 2))" > /tmp/result-$TASK_ARRAY_INDEX.txt
'
```

**Expected output:**
```
🚀 Launching job array...

Array Size: 5
Instance Type: t3.micro
TTL: 30m

Progress:
  ✓ Creating 5 instances
  ✓ Launching instances...

Instances:
  my-array-0 → i-0abc000 (launching)
  my-array-1 → i-0abc001 (launching)
  my-array-2 → i-0abc002 (launching)
  my-array-3 → i-0abc003 (launching)
  my-array-4 → i-0abc004 (launching)

Job array launched! 🎉

Array ID: array-20260127-100000
Instances: 5

Monitor:
  spawn list --tag array:id=array-20260127-100000

Estimated cost: ~$0.03 (5 instances × 30 minutes × $0.0104/hour)
```

### Step 2: Verify Environment Variables

Connect to one instance:

```bash
spawn connect my-array-0
```

**Check variables:**
```bash
echo $TASK_ARRAY_INDEX
# Shows: 0

echo $TASK_ARRAY_SIZE
# Shows: 5

cat /tmp/result-$TASK_ARRAY_INDEX.txt
# Shows: Result: 0

exit
```

Connect to different instance:

```bash
spawn connect my-array-3

echo $TASK_ARRAY_INDEX
# Shows: 3

exit
```

✅ Each instance has unique index!

## Real-World Example: Process 100 Files

Process 100 CSV files in parallel.

### Scenario

You have 100 data files on S3:
```
s3://my-data/input/file-000.csv
s3://my-data/input/file-001.csv
...
s3://my-data/input/file-099.csv
```

Each instance processes one file.

### Step 1: Create Processing Script

```bash
cat > process.sh << 'EOF'
#!/bin/bash
set -e

# Get array index
INDEX=$TASK_ARRAY_INDEX

# Pad index to 3 digits (000, 001, ..., 099)
PADDED_INDEX=$(printf "%03d" $INDEX)

# File paths
INPUT_FILE="s3://my-data/input/file-${PADDED_INDEX}.csv"
OUTPUT_FILE="s3://my-data/output/file-${PADDED_INDEX}.csv"

echo "Processing file ${PADDED_INDEX}"

# Download input
aws s3 cp "$INPUT_FILE" /tmp/input.csv

# Process (replace with your logic)
# Example: Convert to uppercase
tr '[:lower:]' '[:upper:]' < /tmp/input.csv > /tmp/output.csv

# Upload output
aws s3 cp /tmp/output.csv "$OUTPUT_FILE"

echo "Completed file ${PADDED_INDEX}"

# Mark complete
spored complete --status success
EOF

chmod +x process.sh
```

### Step 2: Launch Array

```bash
spawn launch \
  --instance-type c7i.large \
  --array 100 \
  --ttl 2h \
  --name file-processor \
  --iam-policy s3:FullAccess \
  --on-complete terminate \
  --user-data @process.sh
```

**What this does:**
- Launches 100 instances
- Each gets unique index (0-99)
- Each processes one file
- Auto-terminates when done

**Expected output:**
```
🚀 Launching job array...

Array Size: 100
Instance Type: c7i.large
TTL: 2h

Launching 100 instances...
  ⠋ Progress: 23/100 launched

...

Job array launched! 🎉

Array ID: array-files-20260127-103000
Instances: 100

Estimated cost: ~$3.50 (100 instances × 2 hours × $0.0175/hour)
Spot cost: ~$1.00 (70% savings)
```

### Step 3: Monitor Progress

```bash
# Watch overall progress
spawn list --tag array:id=array-files-20260127-103000 --state running

# Count completed
spawn list --tag array:id=array-files-20260127-103000 --state terminated | wc -l

# Cost so far
spawn cost --tag array:id=array-files-20260127-103000
```

**Track completion over time:**
```bash
watch -n 10 'echo "Running: $(spawn list --tag array:id=array-files-20260127-103000 --state running | wc -l)/100"'
```

**Output:**
```
Running: 87/100
Running: 62/100
Running: 34/100
Running: 8/100
Running: 0/100  # All done!
```

## Real-World Example: Video Rendering

Render 500 video frames in parallel.

### Scenario

Render frames 0-499 of a video.

### Step 1: Create Render Script

```bash
cat > render.sh << 'EOF'
#!/bin/bash
set -e

# Get frame number from array index
FRAME=$TASK_ARRAY_INDEX

echo "Rendering frame $FRAME"

# Download scene file
aws s3 cp s3://my-renders/scene.blend /tmp/scene.blend

# Render frame using Blender
blender -b /tmp/scene.blend -f $FRAME -o /tmp/frame-####

# Upload rendered frame
aws s3 cp /tmp/frame-$(printf "%04d" $FRAME).png \
  s3://my-renders/output/frame-$(printf "%04d" $FRAME).png

echo "Completed frame $FRAME"
spored complete --status success
EOF
```

### Step 2: Launch Render Array

```bash
spawn launch \
  --instance-type c7i.2xlarge \
  --array 500 \
  --ttl 1h \
  --name video-render \
  --ami ami-blender \
  --iam-policy s3:FullAccess \
  --spot \
  --spot-max-price 0.15 \
  --on-complete terminate \
  --user-data @render.sh
```

**Cost comparison:**
```
On-demand: 500 × 1h × $0.35/h = $175
Spot:      500 × 1h × $0.10/h = $50
Savings: $125 (71%)
```

## Real-World Example: Monte Carlo Simulation

Run 1000 Monte Carlo simulations.

### Step 1: Create Simulation Script

```bash
cat > simulate.py << 'EOF'
#!/usr/bin/env python3
import os
import random
import json

# Get simulation number
sim_id = int(os.environ['TASK_ARRAY_INDEX'])

# Set seed for reproducibility
random.seed(sim_id)

# Run simulation (simplified example)
results = []
for _ in range(10000):
    value = random.gauss(100, 15)
    results.append(value)

mean = sum(results) / len(results)
variance = sum((x - mean) ** 2 for x in results) / len(results)

# Save results
output = {
    'simulation_id': sim_id,
    'mean': mean,
    'variance': variance
}

with open(f'/tmp/simulation-{sim_id:04d}.json', 'w') as f:
    json.dump(output, f, indent=2)

# Upload to S3
import subprocess
subprocess.run([
    'aws', 's3', 'cp',
    f'/tmp/simulation-{sim_id:04d}.json',
    f's3://my-simulations/results/simulation-{sim_id:04d}.json'
])

print(f"Simulation {sim_id} complete: mean={mean:.2f}, var={variance:.2f}")
EOF
```

### Step 2: Launch Simulation Array

```bash
spawn launch \
  --instance-type t3.medium \
  --array 1000 \
  --ttl 30m \
  --name monte-carlo \
  --iam-policy s3:FullAccess \
  --spot \
  --on-complete terminate \
  --user-data '#!/bin/bash
yum install -y python3
python3 /home/ec2-user/simulate.py
spored complete --status success
'
```

### Step 3: Aggregate Results

After all simulations complete:

```bash
# Download all results
aws s3 sync s3://my-simulations/results/ results/

# Aggregate in Python
python3 << 'EOF'
import json
import glob

results = []
for file in glob.glob('results/simulation-*.json'):
    with open(file) as f:
        results.append(json.load(f))

# Calculate aggregate statistics
means = [r['mean'] for r in results]
overall_mean = sum(means) / len(means)

print(f"Ran {len(results)} simulations")
print(f"Overall mean: {overall_mean:.2f}")
EOF
```

## Optimizing Costs

### 1. Use Spot Instances

```bash
spawn launch --array 100 --spot --spot-max-price 0.05
```

**Savings:** Up to 70%

### 2. Right-Size Instances

```bash
# Too large (wasteful)
--instance-type m7i.xlarge  # $0.1680/hour

# Right size
--instance-type t3.medium  # $0.0416/hour
```

**Savings:** 75% ($0.1264/hour per instance)

### 3. Set Accurate TTL

```bash
# Task takes 10 minutes, set TTL to 15 minutes
--ttl 15m

# Not: --ttl 2h (wasteful if task finishes early)
```

**Use `--on-complete terminate`** to terminate immediately when done.

### 4. Batch Processing

Instead of:
```bash
# 1000 instances, 1 file each
spawn launch --array 1000
```

Consider:
```bash
# 100 instances, 10 files each
spawn launch --array 100
```

Each instance processes multiple files:
```bash
BATCH_SIZE=10
START=$((TASK_ARRAY_INDEX * BATCH_SIZE))
END=$((START + BATCH_SIZE))

for i in $(seq $START $((END - 1))); do
  process_file $i
done
```

## Advanced Patterns

### Dynamic Task Distribution

Use SQS queue for dynamic task assignment:

```bash
# Instance pulls tasks from queue until empty
while true; do
  TASK=$(aws sqs receive-message --queue-url $QUEUE_URL ...)
  if [ -z "$TASK" ]; then
    break
  fi
  process_task "$TASK"
  aws sqs delete-message --queue-url $QUEUE_URL ...
done

spored complete --status success
```

Benefits:
- Handles variable task durations
- Auto-balances load
- Efficient resource usage

### Chunked Arrays

Process 10,000 files with 100 instances (100 files per instance):

```bash
spawn launch --array 100 --user-data '#!/bin/bash
CHUNK_SIZE=100
START=$((TASK_ARRAY_INDEX * CHUNK_SIZE))
END=$((START + CHUNK_SIZE))

for i in $(seq $START $((END - 1))); do
  aws s3 cp s3://data/file-$(printf "%05d" $i).csv /tmp/input.csv
  process_file.py /tmp/input.csv
  aws s3 cp /tmp/output.csv s3://results/file-$(printf "%05d" $i).csv
done

spored complete --status success
'
```

### Retry Failed Tasks

Track failed tasks and relaunch:

```bash
# Launch initial array
spawn launch --array 100 --name task-batch-1

# After completion, check for failures
FAILED_INDICES=$(check_s3_for_missing_outputs)

# Relaunch only failed tasks
for index in $FAILED_INDICES; do
  spawn launch --name retry-$index --user-data "TASK_ARRAY_INDEX=$index; ./process.sh"
done
```

## Monitoring Large Arrays

### Progress Dashboard

```bash
cat > monitor.sh << 'EOF'
#!/bin/bash
ARRAY_ID=$1

while true; do
  clear
  echo "Job Array: $ARRAY_ID"
  echo "================================"

  TOTAL=$(spawn list --tag array:id=$ARRAY_ID | wc -l)
  RUNNING=$(spawn list --tag array:id=$ARRAY_ID --state running | wc -l)
  STOPPED=$(spawn list --tag array:id=$ARRAY_ID --state stopped | wc -l)
  TERMINATED=$(spawn list --tag array:id=$ARRAY_ID --state terminated | wc -l)

  echo "Total:      $TOTAL"
  echo "Running:    $RUNNING"
  echo "Stopped:    $STOPPED"
  echo "Terminated: $TERMINATED"
  echo ""
  echo "Progress: $TERMINATED / $TOTAL ($(( TERMINATED * 100 / TOTAL ))%)"

  sleep 10
done
EOF

chmod +x monitor.sh
./monitor.sh array-20260127-103000
```

### Cost Tracking

```bash
# Real-time cost
spawn cost --tag array:id=array-20260127-103000

# Cost per hour
watch -n 3600 'spawn cost --tag array:id=array-20260127-103000'
```

### Alerts

Set up completion alert:

```bash
spawn alerts create slack \
  --webhook $SLACK_WEBHOOK \
  --event array_complete \
  --filter array:id=array-20260127-103000
```

## Troubleshooting

### Some Tasks Failed

Find failed instances:

```bash
# List all instances
spawn list --tag array:id=array-xxx

# Check logs
spawn connect <failed-instance-id>
tail -100 /var/log/cloud-init-output.log
```

**Common causes:**
- File not found on S3
- Out of memory
- Script error

**Solution:** Fix script and relaunch failed tasks.

### Array Too Large

AWS has limits (default: 20 vCPUs per region).

**Solution 1:** Request limit increase

**Solution 2:** Launch in batches
```bash
# Launch 100 instances at a time
for batch in {0..9}; do
  spawn launch --array 100 --name batch-$batch
  sleep 300  # Wait 5 minutes between batches
done
```

**Solution 3:** Use multiple regions
```bash
spawn launch --array 50 --region us-east-1
spawn launch --array 50 --region us-west-2
```

### High Costs

**Check estimated cost before launching:**
```bash
spawn launch --array 100 --dry-run
```

**Use spot instances:**
```bash
--spot --spot-max-price 0.10
```

**Set budget alerts:**
```bash
spawn alerts create cost --threshold 50.00
```

## Best Practices

### 1. Test with Small Array First

```bash
# Test with 2-3 instances
spawn launch --array 3 --user-data @script.sh

# After verifying, scale up
spawn launch --array 100 --user-data @script.sh
```

### 2. Use Spot for Batch Work

```bash
spawn launch --array 100 --spot
```

Spot instances are perfect for job arrays (non-urgent, parallel, retriable).

### 3. Set `on_complete: terminate`

```bash
spawn launch --array 100 --on-complete terminate
```

Automatically terminate when work completes.

### 4. Upload Results to S3

More reliable than SSH collection:
```bash
aws s3 cp /tmp/result.txt s3://results/result-$TASK_ARRAY_INDEX.txt
```

### 5. Use Meaningful Names

```bash
# Good
--name file-processor-batch-1

# Bad
--name array
```

### 6. Monitor Costs

```bash
spawn cost --tag array:id=array-xxx
```

## What You Learned

Congratulations! You now understand:

✅ Job arrays vs parameter sweeps
✅ Array indices and environment variables
✅ Batch file processing patterns
✅ Monte Carlo simulation workflows
✅ Cost optimization strategies
✅ Dynamic task distribution
✅ Monitoring large arrays

## Practice Exercises

### Exercise 1: Simple Array

Launch a 10-instance array that prints its index and hostname.

### Exercise 2: File Processing

Create a 5-instance array that processes files 0-4 from S3.

### Exercise 3: Cost Optimization

Compare costs:
- 100 on-demand instances
- 100 spot instances
- 50 instances processing 2 files each

Calculate savings for each approach.

## Next Steps

Continue your learning journey:

📖 **[Tutorial 5: Batch Queues](05-batch-queues.md)** - Sequential job execution

🛠️ **[How-To: Job Arrays](../how-to/job-arrays.md)** - Advanced patterns

📚 **[Command Reference: launch](../reference/commands/launch.md)** - Complete flag documentation

## Quick Reference

```bash
# Launch job array
spawn launch --array 100 --user-data @script.sh

# Array environment variables
$TASK_ARRAY_INDEX  # 0, 1, 2, ..., 99
$TASK_ARRAY_SIZE   # 100

# Monitor array
spawn list --tag array:id=<array-id>

# Cost tracking
spawn cost --tag array:id=<array-id>

# Process file by index
FILE_INDEX=$(printf "%03d" $TASK_ARRAY_INDEX)
aws s3 cp s3://data/file-${FILE_INDEX}.csv /tmp/input.csv
```

---

**Previous:** [← Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md)
**Next:** [Tutorial 5: Batch Queues](05-batch-queues.md) →
