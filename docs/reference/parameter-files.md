# Parameter Files Reference

Complete specification for parameter sweep configuration files.

## Overview

Parameter files define sweeps for launching multiple instances with different configurations. They use YAML format and support both simple lists and complex parameter combinations.

**File format:** YAML (`.yaml` or `.yml`)

**Used by:**
- `spawn launch --param-file <file>`
- `spawn list-sweeps`
- `spawn resume --sweep-id <id>`

## Basic Structure

```yaml
defaults:
  instance_type: t3.micro
  region: us-east-1
  ttl: 2h

params:
  - name: run-1
    learning_rate: 0.001
    batch_size: 32
  - name: run-2
    learning_rate: 0.01
    batch_size: 64
```

## Sections

### defaults
**Type:** Object
**Required:** No
**Description:** Default values applied to all parameters.

**Supported Fields:**
- `instance_type` - EC2 instance type
- `region` - AWS region
- `ttl` - Time to live (duration string)
- `idle_timeout` - Idle timeout
- `ami` - AMI ID
- `spot` - Use spot instances (boolean)
- `iam_policy` - IAM policy templates
- `user_data` - User data script
- Any other launch flags

```yaml
defaults:
  instance_type: m7i.large
  region: us-east-1
  ttl: 8h
  idle_timeout: 1h
  iam_policy: s3:ReadOnly,logs:WriteOnly
```

### params
**Type:** Array of objects
**Required:** Yes
**Description:** List of parameter combinations to launch.

**Required Fields:**
- `name` - Unique parameter name/ID

**Optional Fields:**
- Any field from defaults (overrides default)
- Custom parameters passed to instances

```yaml
params:
  - name: run-001
    learning_rate: 0.001
    batch_size: 32
  - name: run-002
    learning_rate: 0.01
    batch_size: 64
    # Override default
    instance_type: g5.xlarge
```

## Complete Example

```yaml
# sweep.yaml - Hyperparameter tuning

defaults:
  instance_type: g5.xlarge
  region: us-east-1
  ami: ami-pytorch
  ttl: 4h
  idle_timeout: 1h
  iam_policy: s3:FullAccess,logs:WriteOnly
  user_data: |
    #!/bin/bash
    cd /app
    python train.py \
      --learning-rate $LEARNING_RATE \
      --batch-size $BATCH_SIZE \
      --model-dir s3://my-bucket/models/$NAME

params:
  # Learning rate sweep
  - name: lr-0.0001-bs-32
    learning_rate: 0.0001
    batch_size: 32

  - name: lr-0.0001-bs-64
    learning_rate: 0.0001
    batch_size: 64

  - name: lr-0.001-bs-32
    learning_rate: 0.001
    batch_size: 32

  - name: lr-0.001-bs-64
    learning_rate: 0.001
    batch_size: 64

  - name: lr-0.01-bs-32
    learning_rate: 0.01
    batch_size: 32

  - name: lr-0.01-bs-64
    learning_rate: 0.01
    batch_size: 64
```

## Environment Variables

Parameters are exposed as environment variables on instances:

```yaml
params:
  - name: run-1
    learning_rate: 0.001
    batch_size: 32
```

**Instance receives:**
```bash
NAME="run-1"
LEARNING_RATE="0.001"
BATCH_SIZE="32"
```

**Variable naming:**
- Lowercase keys converted to UPPERCASE
- Hyphens converted to underscores
- `learning_rate` → `LEARNING_RATE`
- `model-name` → `MODEL_NAME`

## Advanced Examples

### Cartesian Product Grid
```yaml
# Generate all combinations
defaults:
  instance_type: t3.micro

# Script generates params:
# params.append({
#   'name': f'lr-{lr}-bs-{bs}',
#   'learning_rate': lr,
#   'batch_size': bs
# })
# for lr in [0.001, 0.01, 0.1]
# for bs in [32, 64, 128]
```

### Multi-Region Sweep
```yaml
defaults:
  instance_type: m7i.large
  ttl: 2h

params:
  - name: us-east-1-run
    region: us-east-1
  - name: us-west-2-run
    region: us-west-2
  - name: eu-west-1-run
    region: eu-west-1
```

### Different Instance Types
```yaml
defaults:
  ttl: 4h

params:
  - name: small-model
    instance_type: t3.large
    model_size: small

  - name: large-model
    instance_type: g5.xlarge
    model_size: large
```

## JSON Format

```json
{
  "defaults": {
    "instance_type": "t3.micro",
    "region": "us-east-1",
    "ttl": "2h"
  },
  "params": [
    {
      "name": "run-1",
      "learning_rate": 0.001,
      "batch_size": 32
    },
    {
      "name": "run-2",
      "learning_rate": 0.01,
      "batch_size": 64
    }
  ]
}
```

## Validation

spawn validates parameter files before launching:

- ✅ Required fields present (`params`, each `name`)
- ✅ Valid instance types
- ✅ Valid regions
- ✅ Valid TTL format
- ✅ Unique parameter names
- ⚠️ Warns on unknown fields

## Best Practices

### 1. Use Descriptive Names
```yaml
# Good
name: lr-0.001-bs-32-dropout-0.5

# Bad
name: run-1
```

### 2. Set Defaults
```yaml
# Put common values in defaults
defaults:
  instance_type: m7i.large
  ttl: 4h
  region: us-east-1
```

### 3. Include Metadata
```yaml
params:
  - name: experiment-1
    learning_rate: 0.001
    description: "Baseline model"
    tags: baseline,initial
```

### 4. Validate Before Launch
```bash
# Check parameter file
spawn validate-params sweep.yaml
```

## See Also

- [spawn launch](commands/launch.md) - Launch parameter sweeps
- [spawn status](commands/status.md) - Monitor sweeps
- [spawn collect-results](commands/collect-results.md) - Collect results
