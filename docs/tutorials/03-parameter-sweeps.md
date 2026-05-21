# Tutorial 3: Parameter Sweeps

**Duration:** 30 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

## What You'll Learn

In this tutorial, you'll learn how to launch multiple instances with different configurations:
- Create parameter sweep files (YAML/JSON)
- Launch dozens of instances simultaneously
- Monitor sweep progress
- Collect results from all instances
- Use parameter sweeps for ML hyperparameter tuning
- Use parameter sweeps for batch data processing

## Why Parameter Sweeps?

**Problem:** You need to train 20 ML models with different hyperparameters, or process 100 data files in parallel.

**Without spawn:**
- Launch instances manually one-by-one (error-prone)
- Write custom orchestration scripts
- Track instance IDs manually
- Collect results individually

**With spawn parameter sweeps:**
```bash
spawn launch --param-file sweep.yaml
```
- Launches all instances automatically
- Unique parameters injected into each instance
- Built-in progress tracking
- One-command result collection

## Understanding Parameter Sweeps

A parameter sweep consists of:

1. **Parameter File** - YAML or JSON defining configurations
2. **Sweep ID** - Unique identifier for the group
3. **Environment Variables** - Parameters injected into instances
4. **Results Collection** - Automatic gathering of outputs

**Key Concept:** Each parameter combination launches one instance.

**Example:**
```yaml
params:
  - name: run-1
    learning_rate: 0.001
  - name: run-2
    learning_rate: 0.01
```
= 2 instances launched, each with different `LEARNING_RATE` environment variable

## Creating Your First Parameter File

Let's start with a simple example: testing Python scripts with different configurations.

### Step 1: Create Parameter File

```bash
cd ~
cat > simple-sweep.yaml << 'EOF'
defaults:
  instance_type: t3.micro
  region: us-east-1
  ttl: 30m
  user_data: |
    #!/bin/bash
    echo "Running experiment: $NAME"
    echo "Parameter value: $PARAM_VALUE"
    echo "Result: $(($PARAM_VALUE * 2))" > /tmp/result.txt

params:
  - name: test-1
    param_value: 10
  - name: test-2
    param_value: 20
  - name: test-3
    param_value: 30
EOF
```

**What this does:**
- `defaults` - Applied to all instances
- `params` - 3 different configurations
- `user_data` - Script runs on each instance
- Environment variables: `$NAME`, `$PARAM_VALUE`

### Step 2: Launch the Sweep

```bash
spawn launch --param-file simple-sweep.yaml
```

**Expected output:**
```
🚀 Launching parameter sweep...

Parameter File: simple-sweep.yaml
Sweep ID: sweep-20260127-100000
Parameters: 3
Instances: 3

Configuration:
  Instance Type: t3.micro
  Region: us-east-1
  TTL: 30m

Progress:
  ✓ Validating parameter file
  ✓ Generating 3 instance configurations
  ✓ Launching instances...

Instances:
  test-1 → i-0abc123def456789 (launching)
  test-2 → i-0abc234def567890 (launching)
  test-3 → i-0abc345def678901 (launching)

Sweep launched successfully! 🎉

Sweep ID: sweep-20260127-100000
Instances: 3

Monitor:
  spawn status sweep-20260127-100000

Collect Results:
  spawn collect-results sweep-20260127-100000

Estimated cost: ~$0.03 (3 instances × 30 minutes × $0.0104/hour)
```

### Step 3: Monitor Progress

```bash
spawn status sweep-20260127-100000
```

**Expected output:**
```
Sweep: sweep-20260127-100000
Parameters: 3
Status: running

Instances:
NAME     INSTANCE-ID          STATE     UPTIME  TTL-REMAINING
test-1   i-0abc123def456789   running   2m      28m
test-2   i-0abc234def567890   running   2m      28m
test-3   i-0abc345def678901   running   2m      28m

Progress: 3/3 running (100%)

Costs:
  Current: $0.001
  Projected 30m: $0.016
```

### Step 4: Collect Results

Wait for instances to finish (or SSH in to verify):

```bash
# Check one instance
spawn connect i-0abc123def456789
cat /tmp/result.txt
# Shows: Result: 20
exit

# Collect all results
spawn collect-results sweep-20260127-100000 --output results/
```

**Expected output:**
```
Collecting results from sweep sweep-20260127-100000...

  ✓ test-1 (i-0abc123def456789) → results/test-1/
  ✓ test-2 (i-0abc234def567890) → results/test-2/
  ✓ test-3 (i-0abc345def678901) → results/test-3/

Results collected: 3/3

Output directory: results/
```

## Real-World Example: ML Hyperparameter Tuning

Let's create a realistic ML training sweep.

### Step 1: Create Training Script

```bash
cat > train.py << 'EOF'
#!/usr/bin/env python3
import os
import time
import json

# Get parameters from environment
name = os.environ['NAME']
learning_rate = float(os.environ['LEARNING_RATE'])
batch_size = int(os.environ['BATCH_SIZE'])

print(f"Training {name}")
print(f"  Learning Rate: {learning_rate}")
print(f"  Batch Size: {batch_size}")

# Simulate training
time.sleep(10)

# Fake accuracy based on parameters
accuracy = 0.5 + (learning_rate * 10) + (batch_size / 1000)
accuracy = min(0.95, accuracy)

# Save results
result = {
    'name': name,
    'learning_rate': learning_rate,
    'batch_size': batch_size,
    'accuracy': accuracy
}

with open('/tmp/results.json', 'w') as f:
    json.dump(result, f, indent=2)

print(f"Final accuracy: {accuracy:.3f}")
EOF

chmod +x train.py
```

### Step 2: Create Sweep Configuration

```bash
cat > ml-sweep.yaml << 'EOF'
defaults:
  instance_type: t3.medium
  region: us-east-1
  ttl: 1h
  iam_policy: s3:FullAccess,logs:WriteOnly
  user_data: |
    #!/bin/bash
    set -e

    # Install Python
    yum install -y python3

    # Download training script
    cat > /home/ec2-user/train.py << 'SCRIPT'
    #!/usr/bin/env python3
    import os
    import time
    import json

    name = os.environ['NAME']
    learning_rate = float(os.environ['LEARNING_RATE'])
    batch_size = int(os.environ['BATCH_SIZE'])

    print(f"Training {name}")
    print(f"  LR: {learning_rate}, BS: {batch_size}")

    time.sleep(10)

    accuracy = 0.5 + (learning_rate * 10) + (batch_size / 1000)
    accuracy = min(0.95, accuracy)

    result = {
        'name': name,
        'learning_rate': learning_rate,
        'batch_size': batch_size,
        'accuracy': accuracy
    }

    with open('/tmp/results.json', 'w') as f:
        json.dump(result, f, indent=2)

    print(f"Final accuracy: {accuracy:.3f}")

    # Upload to S3
    aws s3 cp /tmp/results.json s3://my-ml-results/$NAME.json

    # Mark complete
    spored complete --status success
    SCRIPT

    chmod +x /home/ec2-user/train.py
    su - ec2-user -c "python3 /home/ec2-user/train.py"

params:
  # Grid search: 3 learning rates × 3 batch sizes = 9 experiments

  - name: lr-0.001-bs-32
    learning_rate: 0.001
    batch_size: 32

  - name: lr-0.001-bs-64
    learning_rate: 0.001
    batch_size: 64

  - name: lr-0.001-bs-128
    learning_rate: 0.001
    batch_size: 128

  - name: lr-0.01-bs-32
    learning_rate: 0.01
    batch_size: 32

  - name: lr-0.01-bs-64
    learning_rate: 0.01
    batch_size: 64

  - name: lr-0.01-bs-128
    learning_rate: 0.01
    batch_size: 128

  - name: lr-0.1-bs-32
    learning_rate: 0.1
    batch_size: 32

  - name: lr-0.1-bs-64
    learning_rate: 0.1
    batch_size: 64

  - name: lr-0.1-bs-128
    learning_rate: 0.1
    batch_size: 128
EOF
```

### Step 3: Launch ML Sweep

```bash
spawn launch --param-file ml-sweep.yaml --on-complete terminate
```

**What this does:**
- Launches 9 instances (one per parameter combination)
- Each runs training script with unique parameters
- Results uploaded to S3
- Instances auto-terminate on completion

**Expected output:**
```
🚀 Launching parameter sweep...

Sweep ID: sweep-ml-20260127-101500
Parameters: 9
Instances: 9

Launching instances:
  lr-0.001-bs-32   → i-xxx (launching)
  lr-0.001-bs-64   → i-xxx (launching)
  lr-0.001-bs-128  → i-xxx (launching)
  ...

Sweep launched: 9 instances

Estimated cost: ~$0.10 (9 instances × 1 hour × $0.0116/hour)
```

### Step 4: Monitor Training

```bash
# Watch overall progress
watch -n 5 'spawn status sweep-ml-20260127-101500'

# Connect to one instance to watch training
spawn connect i-xxx
tail -f /var/log/cloud-init-output.log
```

### Step 5: Analyze Results

```bash
# Collect all results
spawn collect-results sweep-ml-20260127-101500 --output ml-results/

# Find best hyperparameters
cd ml-results/
for dir in */; do
  cat "$dir/results.json"
done | jq -s 'sort_by(.accuracy) | reverse | .[0]'
```

**Expected output:**
```json
{
  "name": "lr-0.01-bs-64",
  "learning_rate": 0.01,
  "batch_size": 64,
  "accuracy": 0.924
}
```

## Real-World Example: Batch Data Processing

Process 50 data files in parallel.

### Create Processing Sweep

```bash
cat > batch-sweep.yaml << 'EOF'
defaults:
  instance_type: c7i.large
  region: us-east-1
  ttl: 2h
  iam_policy: s3:FullAccess
  on_complete: terminate
  user_data: |
    #!/bin/bash
    set -e

    INPUT_FILE="s3://my-data-bucket/input/$FILE_NAME"
    OUTPUT_FILE="s3://my-data-bucket/output/$FILE_NAME"

    # Download input
    aws s3 cp "$INPUT_FILE" /tmp/input.csv

    # Process (replace with your processing)
    python3 /app/process.py /tmp/input.csv /tmp/output.csv

    # Upload output
    aws s3 cp /tmp/output.csv "$OUTPUT_FILE"

    # Mark complete
    spored complete --status success

params:
  - name: file-001
    file_name: data-001.csv
  - name: file-002
    file_name: data-002.csv
  - name: file-003
    file_name: data-003.csv
  # ... continue for all 50 files
EOF
```

### Generate Parameter File Programmatically

For many files, generate parameter file with script:

```python
#!/usr/bin/env python3
import yaml

# Generate 50 parameter entries
params = []
for i in range(1, 51):
    params.append({
        'name': f'file-{i:03d}',
        'file_name': f'data-{i:03d}.csv'
    })

sweep_config = {
    'defaults': {
        'instance_type': 'c7i.large',
        'region': 'us-east-1',
        'ttl': '2h',
        'iam_policy': 's3:FullAccess',
        'on_complete': 'terminate',
        'user_data': '''#!/bin/bash
aws s3 cp s3://my-bucket/input/$FILE_NAME /tmp/input.csv
python3 /app/process.py /tmp/input.csv /tmp/output.csv
aws s3 cp /tmp/output.csv s3://my-bucket/output/$FILE_NAME
spored complete --status success
'''
    },
    'params': params
}

with open('batch-sweep.yaml', 'w') as f:
    yaml.dump(sweep_config, f, default_flow_style=False)

print(f"Generated sweep with {len(params)} parameters")
```

```bash
python3 generate-sweep.py
spawn launch --param-file batch-sweep.yaml
```

## Monitoring Large Sweeps

For large sweeps (50+ instances), use specialized commands:

```bash
# List all sweeps
spawn list-sweeps

# Detailed status
spawn status sweep-xxx

# Filter by state
spawn list --tag sweep:id=sweep-xxx --state running

# Count completed
spawn list --tag sweep:id=sweep-xxx --state terminated | wc -l

# Cost so far
spawn cost --tag sweep:id=sweep-xxx
```

## Collecting Results

Multiple collection methods:

### Method 1: Automatic Collection

```bash
# Collect all results to local directory
spawn collect-results sweep-xxx --output results/
```

### Method 2: S3 Results

If instances upload to S3, download from S3:

```bash
aws s3 sync s3://my-results/sweep-xxx/ results/
```

### Method 3: SSH and SCP

For individual instances:

```bash
# List instances in sweep
INSTANCES=$(spawn list --tag sweep:id=sweep-xxx --format json | jq -r '.[].instance_id')

# Collect from each
for instance in $INSTANCES; do
  name=$(spawn status "$instance" --json | jq -r '.tags.Name')
  scp "ec2-user@$(spawn status "$instance" --json | jq -r '.network.public_ip'):/tmp/results.json" "results/$name.json"
done
```

## Best Practices

### 1. Start Small, Scale Up

```bash
# Test with 2-3 parameters first
params:
  - name: test-1
    value: 1
  - name: test-2
    value: 2

# After verifying, scale to full sweep
```

### 2. Use Descriptive Names

```bash
# Good
name: lr-0.001-bs-32-dropout-0.5-seed-42

# Bad
name: run-1
```

### 3. Set Appropriate TTL

```bash
# Short jobs: 30m-1h
ttl: 1h

# ML training: 4-12h
ttl: 8h

# Long batch processing: 24h+
ttl: 24h
```

### 4. Use `on_complete: terminate`

```bash
# Automatically terminate when script finishes
defaults:
  on_complete: terminate
```

Add to user_data:
```bash
spored complete --status success
```

### 5. Upload Results to S3

```bash
# In user_data script
aws s3 cp /tmp/results.json s3://my-bucket/results/$NAME.json
```

More reliable than collecting over SSH.

### 6. Use Spot Instances for Cost Savings

```bash
defaults:
  spot: true
  spot_max_price: 0.10
```

Can save up to 70% on large sweeps.

### 7. Monitor Costs

```bash
# Before launching
spawn launch --param-file sweep.yaml --dry-run

# During execution
spawn cost --tag sweep:id=sweep-xxx

# Set budget alert
spawn alerts create cost --threshold 10.00
```

## Advanced: Cartesian Product Sweeps

Generate all combinations of parameters:

```python
#!/usr/bin/env python3
import yaml
import itertools

learning_rates = [0.001, 0.01, 0.1]
batch_sizes = [32, 64, 128]
dropouts = [0.0, 0.2, 0.5]

params = []
for lr, bs, dropout in itertools.product(learning_rates, batch_sizes, dropouts):
    params.append({
        'name': f'lr-{lr}-bs-{bs}-dropout-{dropout}',
        'learning_rate': lr,
        'batch_size': bs,
        'dropout': dropout
    })

print(f"Generated {len(params)} parameter combinations")
# 3 × 3 × 3 = 27 combinations
```

## Troubleshooting

### Some Instances Fail

Check failed instances:

```bash
# List all instances
spawn list --tag sweep:id=sweep-xxx

# Check logs for failed instance
spawn connect i-xxx
tail -100 /var/log/cloud-init-output.log
```

### Sweep Too Expensive

```bash
# Use smaller instance types
defaults:
  instance_type: t3.micro  # Instead of m7i.large

# Use spot instances
defaults:
  spot: true

# Reduce TTL
defaults:
  ttl: 30m  # Instead of 4h
```

### Results Not Collected

Make sure script writes to accessible location:

```bash
# Good: Write to /tmp
echo "result" > /tmp/result.txt

# Better: Upload to S3
aws s3 cp /tmp/result.txt s3://my-bucket/results/$NAME.txt
```

## What You Learned

Congratulations! You now understand:

✅ What parameter sweeps are and when to use them
✅ How to create parameter files (YAML/JSON)
✅ How to launch sweeps with multiple instances
✅ How to monitor sweep progress
✅ How to collect results from all instances
✅ ML hyperparameter tuning workflows
✅ Batch data processing patterns
✅ Cost optimization for large sweeps

## Practice Exercises

### Exercise 1: Simple Sweep

Create a 3-parameter sweep that echoes different messages:

```yaml
params:
  - name: greeting-1
    message: "Hello"
  - name: greeting-2
    message: "Hola"
  - name: greeting-3
    message: "Bonjour"
```

### Exercise 2: ML Grid Search

Create a 2×2 grid search (2 learning rates, 2 batch sizes = 4 experiments).

### Exercise 3: Cost Comparison

Compare spot vs on-demand for a 10-instance sweep. Calculate savings.

## Next Steps

Continue your learning journey:

📖 **[Tutorial 4: Job Arrays](04-job-arrays.md)** - Launch 100s of identical instances

🛠️ **[How-To: Parameter Sweeps](../how-to/parameter-sweeps.md)** - Advanced patterns and recipes

📚 **[Parameter Files Reference](../reference/parameter-files.md)** - Complete file format documentation

## Quick Reference

```bash
# Launch sweep
spawn launch --param-file sweep.yaml

# Monitor sweep
spawn status <sweep-id>
spawn list --tag sweep:id=<sweep-id>

# Collect results
spawn collect-results <sweep-id> --output results/

# Cancel sweep
spawn cancel <sweep-id>

# Cost tracking
spawn cost --tag sweep:id=<sweep-id>
```

---

**Previous:** [← Tutorial 2: Your First Instance](02-first-instance.md)
**Next:** [Tutorial 4: Job Arrays](04-job-arrays.md) →
