# How-To: Parameter Sweeps

Advanced patterns and techniques for parameter sweeps.

## Generate Large Parameter Files

### Problem
Need to generate 1000+ parameter combinations programmatically.

### Solution: Python Script

```python
#!/usr/bin/env python3
import yaml
import itertools

# Define parameter ranges
learning_rates = [0.0001, 0.0003, 0.001, 0.003, 0.01]
batch_sizes = [16, 32, 64, 128]
optimizers = ['adam', 'sgd', 'rmsprop']
dropouts = [0.0, 0.2, 0.5]

# Generate all combinations
params = []
for lr, bs, opt, drop in itertools.product(learning_rates, batch_sizes, optimizers, dropouts):
    params.append({
        'name': f'lr-{lr}-bs-{bs}-opt-{opt}-drop-{drop}',
        'learning_rate': lr,
        'batch_size': bs,
        'optimizer': opt,
        'dropout': drop
    })

# Create sweep config
sweep = {
    'defaults': {
        'instance_type': 'g5.xlarge',
        'region': 'us-east-1',
        'ttl': '6h',
        'spot': True,
        'iam_policy': 's3:FullAccess,logs:WriteOnly',
        'on_complete': 'terminate'
    },
    'params': params
}

# Write to file
with open('sweep.yaml', 'w') as f:
    yaml.dump(sweep, f, default_flow_style=False)

print(f"Generated {len(params)} parameter combinations")
print(f"Total combinations: {len(learning_rates)} × {len(batch_sizes)} × {len(optimizers)} × {len(dropouts)} = {len(params)}")
```

**Output:**
```
Generated 240 parameter combinations
Total combinations: 5 × 4 × 3 × 4 = 240
```

---

## Random Search Instead of Grid Search

### Problem
Grid search is exhaustive and expensive. Random search often finds good solutions faster.

### Solution: Random Sampling

```python
#!/usr/bin/env python3
import yaml
import random

random.seed(42)

# Generate 100 random parameter combinations
params = []
for i in range(100):
    params.append({
        'name': f'random-{i:03d}',
        'learning_rate': random.uniform(0.0001, 0.01),
        'batch_size': random.choice([16, 32, 64, 128]),
        'dropout': random.uniform(0.0, 0.5),
        'weight_decay': random.uniform(0.0, 0.001)
    })

sweep = {
    'defaults': {
        'instance_type': 'g5.xlarge',
        'ttl': '4h',
        'spot': True
    },
    'params': params
}

with open('random-search.yaml', 'w') as f:
    yaml.dump(sweep, f, default_flow_style=False)

print(f"Generated {len(params)} random parameter combinations")
```

**Benefits:**
- Faster to run (100 runs vs 1000s)
- Often finds good solutions
- Better exploration of parameter space

---

## Hierarchical Parameter Sweeps

### Problem
Want to do coarse search first, then fine-tune around best results.

### Solution: Two-Stage Sweep

**Stage 1: Coarse search**
```yaml
# coarse-sweep.yaml
defaults:
  instance_type: g5.xlarge
  ttl: 2h
  spot: true

params:
  - name: coarse-lr-0.0001
    learning_rate: 0.0001
  - name: coarse-lr-0.001
    learning_rate: 0.001
  - name: coarse-lr-0.01
    learning_rate: 0.01
  - name: coarse-lr-0.1
    learning_rate: 0.1
```

**Launch and analyze:**
```bash
spawn launch --param-file coarse-sweep.yaml
# Wait for completion
spawn collect-results sweep-coarse-xxx

# Find best learning rate
python analyze.py  # Shows lr=0.001 is best
```

**Stage 2: Fine-tune around best**
```yaml
# fine-sweep.yaml
defaults:
  instance_type: g5.xlarge
  ttl: 4h
  spot: true

params:
  - name: fine-lr-0.0005
    learning_rate: 0.0005
  - name: fine-lr-0.0008
    learning_rate: 0.0008
  - name: fine-lr-0.001
    learning_rate: 0.001
  - name: fine-lr-0.0012
    learning_rate: 0.0012
  - name: fine-lr-0.0015
    learning_rate: 0.0015
```

---

## Resume Failed Sweeps

### Problem
Some instances in sweep failed. Don't want to rerun successful ones.

### Solution: Filter and Relaunch

**Check which failed:**
```bash
spawn status sweep-20260127-abc123 --json > sweep-status.json

# Extract failed parameter names
jq -r '.instances[] | select(.exit_code != 0) | .parameter_name' sweep-status.json > failed.txt

# Shows:
# run-023
# run-045
# run-078
```

**Create sweep with only failed parameters:**
```python
#!/usr/bin/env python3
import yaml

# Load original sweep
with open('original-sweep.yaml') as f:
    sweep = yaml.safe_load(f)

# Load failed parameter names
with open('failed.txt') as f:
    failed_names = [line.strip() for line in f]

# Filter to only failed parameters
failed_params = [p for p in sweep['params'] if p['name'] in failed_names]

# Create new sweep
retry_sweep = {
    'defaults': sweep['defaults'],
    'params': failed_params
}

with open('retry-sweep.yaml', 'w') as f:
    yaml.dump(retry_sweep, f, default_flow_style=False)

print(f"Created retry sweep with {len(failed_params)} failed parameters")
```

**Relaunch:**
```bash
spawn launch --param-file retry-sweep.yaml
```

---

## Multi-Region Sweeps

### Problem
Need more capacity or want geographic distribution.

### Solution: Split Across Regions

```python
#!/usr/bin/env python3
import yaml

# Generate 100 parameter combinations
params = [...]  # Your parameter list

# Split into 4 regions
regions = ['us-east-1', 'us-west-2', 'eu-west-1', 'ap-southeast-1']
params_per_region = len(params) // len(regions)

for i, region in enumerate(regions):
    start = i * params_per_region
    end = start + params_per_region if i < len(regions) - 1 else len(params)

    region_sweep = {
        'defaults': {
            'instance_type': 'g5.xlarge',
            'region': region,
            'ttl': '4h',
            'spot': True
        },
        'params': params[start:end]
    }

    with open(f'sweep-{region}.yaml', 'w') as f:
        yaml.dump(region_sweep, f, default_flow_style=False)

    print(f"Region {region}: {end - start} parameters")
```

**Launch all:**
```bash
for region in us-east-1 us-west-2 eu-west-1 ap-southeast-1; do
  spawn launch --param-file sweep-${region}.yaml
done
```

---

## Dynamic Results Upload

### Problem
Want to upload results to S3 as each instance completes.

### Solution: User Data with S3 Upload

```yaml
defaults:
  instance_type: g5.xlarge
  iam_policy: s3:FullAccess
  user_data: |
    #!/bin/bash
    set -e

    # Run training
    cd /app
    python train.py \
      --learning-rate $LEARNING_RATE \
      --batch-size $BATCH_SIZE \
      --output /results

    # Upload results immediately
    aws s3 cp /results/model.pth \
      s3://my-bucket/sweep-results/$NAME/model.pth
    aws s3 cp /results/metrics.json \
      s3://my-bucket/sweep-results/$NAME/metrics.json

    # Mark complete
    spored complete --status success

params:
  - name: run-001
    learning_rate: 0.001
    batch_size: 32
  # ... more params
```

**Monitor results in S3:**
```bash
# Watch results arrive
watch -n 10 'aws s3 ls s3://my-bucket/sweep-results/ | wc -l'
```

---

## Early Stopping

### Problem
Want to stop unpromising experiments early to save money.

### Solution: Checkpoint and Evaluate

```yaml
user_data: |
  #!/bin/bash
  set -e

  cd /app

  # Train with checkpoints
  python train.py \
    --learning-rate $LEARNING_RATE \
    --checkpoint-every 10 \
    --output /checkpoints

  # After 10 epochs, evaluate
  python evaluate.py --checkpoint /checkpoints/epoch-10.pth > /tmp/eval.json

  # Check if validation loss is acceptable
  VAL_LOSS=$(jq -r '.val_loss' /tmp/eval.json)
  THRESHOLD=1.0

  if (( $(echo "$VAL_LOSS > $THRESHOLD" | bc -l) )); then
    echo "Validation loss $VAL_LOSS exceeds threshold $THRESHOLD. Stopping early."
    aws s3 cp /tmp/eval.json s3://results/$NAME/early-stopped.json
    spored complete --status stopped
    exit 0
  fi

  # Continue training
  python train.py \
    --learning-rate $LEARNING_RATE \
    --resume /checkpoints/epoch-10.pth \
    --epochs 100 \
    --output /results

  aws s3 cp /results/model.pth s3://results/$NAME/model.pth
  spored complete --status success
```

---

## Aggregate Results Automatically

### Problem
Have results from 100s of experiments, need to find best.

### Solution: Lambda Trigger on S3

**Setup:**
```bash
# Create Lambda function that triggers on S3 uploads
# Aggregates results and finds best parameters
```

**Or use script:**
```python
#!/usr/bin/env python3
import json
import boto3
from pathlib import Path

s3 = boto3.client('s3')

# Download all results
bucket = 'my-results'
prefix = 'sweep-20260127/'

results = []
paginator = s3.get_paginator('list_objects_v2')
for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
    for obj in page.get('Contents', []):
        if obj['Key'].endswith('metrics.json'):
            # Download and parse
            response = s3.get_object(Bucket=bucket, Key=obj['Key'])
            metrics = json.loads(response['Body'].read())
            results.append(metrics)

# Sort by validation accuracy
results.sort(key=lambda x: x.get('val_accuracy', 0), reverse=True)

# Print top 10
print("Top 10 Results:")
print(f"{'Rank':<6} {'Name':<30} {'Val Acc':<10} {'LR':<10} {'BS':<6}")
print("-" * 70)
for i, r in enumerate(results[:10], 1):
    print(f"{i:<6} {r['name']:<30} {r['val_accuracy']:<10.4f} {r['learning_rate']:<10.6f} {r['batch_size']:<6}")

# Save summary
with open('best-results.json', 'w') as f:
    json.dump(results[:10], f, indent=2)

print(f"\nAnalyzed {len(results)} experiments")
print(f"Best: {results[0]['name']} with {results[0]['val_accuracy']:.4f} accuracy")
```

---

## Parallel Sweeps with Dependencies

### Problem
Want to run multiple sweeps where second sweep uses best params from first.

### Solution: Sequential Execution

```bash
#!/bin/bash
set -e

# Stage 1: Coarse search
echo "Stage 1: Coarse search"
spawn launch --param-file stage1-coarse.yaml
SWEEP1=$(spawn list-sweeps | grep stage1 | awk '{print $1}')

# Wait for completion
while true; do
  STATUS=$(spawn status $SWEEP1 --json | jq -r '.status')
  if [ "$STATUS" = "completed" ]; then
    break
  fi
  echo "Waiting for stage 1... ($STATUS)"
  sleep 60
done

# Collect results and analyze
spawn collect-results $SWEEP1 --output stage1-results/
python analyze-stage1.py  # Generates stage2-fine.yaml

# Stage 2: Fine-tune around best
echo "Stage 2: Fine-tune"
spawn launch --param-file stage2-fine.yaml
SWEEP2=$(spawn list-sweeps | grep stage2 | awk '{print $1}')

# Wait for completion
while true; do
  STATUS=$(spawn status $SWEEP2 --json | jq -r '.status')
  if [ "$STATUS" = "completed" ]; then
    break
  fi
  echo "Waiting for stage 2... ($STATUS)"
  sleep 60
done

# Final results
spawn collect-results $SWEEP2 --output stage2-results/
python analyze-final.py
echo "Sweep pipeline complete!"
```

---

## Cost-Optimized Sweeps

### Problem
Sweep is expensive. Want to optimize cost without sacrificing too much time.

### Solution: Progressive Sizing

```python
#!/usr/bin/env python3
import yaml

# Start with small, cheap instances for quick experiments
# Use larger instances for promising experiments

params = []

# First 20: Quick validation on t3.medium
for i in range(20):
    params.append({
        'name': f'quick-{i:03d}',
        'learning_rate': random.uniform(0.0001, 0.01),
        'instance_type': 't3.medium',
        'epochs': 10  # Just 10 epochs to test
    })

# Next 10: More training on m7i.large
for i in range(10):
    params.append({
        'name': f'medium-{i:03d}',
        'learning_rate': random.uniform(0.001, 0.005),
        'instance_type': 'm7i.large',
        'epochs': 50
    })

# Top 3: Full training on g5.xlarge
for i in range(3):
    params.append({
        'name': f'full-{i:03d}',
        'learning_rate': best_lrs[i],
        'instance_type': 'g5.xlarge',
        'epochs': 100
    })
```

**Cost comparison:**
- All 33 on g5.xlarge: 33 × 4h × $1.006 = $132.79
- Progressive: (20 × 30m × $0.042) + (10 × 2h × $0.10) + (3 × 4h × $1.006) = $14.27
- **Savings: $118.52 (89%)**

---

## See Also

- [Tutorial 3: Parameter Sweeps](../tutorials/03-parameter-sweeps.md) - Learn parameter sweeps
- [Parameter Files Reference](../reference/parameter-files.md) - File format
- [spawn launch](../reference/commands/launch.md) - Launch command
