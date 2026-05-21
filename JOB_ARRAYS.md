# Job Arrays - Coordinated Instance Groups

Job arrays allow you to launch and manage coordinated groups of EC2 instances with automatic peer discovery, perfect for distributed computing, MPI jobs, and parameter sweeps.

## 🎯 Quick Start

```bash
# Launch 8-instance job array
spawn launch --count 8 --job-array-name compute --instance-type m7i.large

# List job arrays (grouped view)
spawn list

# Manage entire array
spawn extend --job-array-name compute 2h
spawn stop --job-array-name compute
spawn start --job-array-name compute
```

## 📋 Table of Contents

- [Overview](#overview)
- [Use Cases](#use-cases)
- [Launching Job Arrays](#launching-job-arrays)
- [Coordination Metadata](#coordination-metadata)
- [Usage Patterns](#usage-patterns)
- [Management Commands](#management-commands)
- [DNS Support](#dns-support)
- [Examples](#examples)
- [Best Practices](#best-practices)

---

## Overview

Job arrays provide **MPI-style coordination** for groups of instances:

- **Automatic peer discovery**: Every instance knows about all peers
- **Rank/index assignment**: Each instance has a unique index (0..N-1)
- **Group DNS**: Single DNS name resolves to all instance IPs
- **Batch management**: Start/stop/extend entire arrays with one command
- **Parallel launch**: All N instances launch simultaneously

### Architecture

```
Launch Phase (Parallel)
┌─────────────────────────────────────────────┐
│  spawn CLI launches all N instances         │
│  Each gets: index, size, job-array-id       │
└─────────────────────────────────────────────┘
                    ↓
Coordination Phase (After All Running)
┌─────────────────────────────────────────────┐
│  spawn CLI collects all peer info           │
│  Updates tags with peer JSON                │
│  spored on each instance writes peer file   │
└─────────────────────────────────────────────┘
                    ↓
User Workload
┌─────────────────────────────────────────────┐
│  MPI: Wait for peers → Run coordinated      │
│  Parallel: Start immediately → Independent  │
└─────────────────────────────────────────────┘
```

---

## Use Cases

### 🔄 Coordinated Workloads (Need Barrier)

Jobs where instances **communicate** with each other:

- **MPI jobs**: Message Passing Interface applications
- **Distributed training**: PyTorch DDP, Horovod, TensorFlow distributed
- **Ray/Dask clusters**: Distributed computing frameworks
- **Distributed databases**: Multi-node database clusters
- **MapReduce**: Coordinated data processing

**Pattern**: Wait for all peers to be ready before starting.

### ⚡ Embarrassingly Parallel (No Barrier)

Jobs where instances work **independently**:

- **Parameter sweeps**: Each instance tries different hyperparameters
- **Monte Carlo simulations**: Independent random trials
- **Batch rendering**: Each instance renders different frames
- **Data processing**: Each instance processes a chunk of data
- **Hyperparameter tuning**: Grid search or random search

**Pattern**: Start immediately using your index to determine work.

---

## Launching Job Arrays

### Basic Syntax

```bash
spawn launch --count N --job-array-name <name> [flags]
```

### Flags

| Flag | Description | Required |
|------|-------------|----------|
| `--count N` | Number of instances | Yes |
| `--job-array-name <name>` | Human-readable group name | Yes |
| `--instance-names <pattern>` | Name template (default: `{job-array-name}-{index}`) | No |
| `--command <cmd>` | Command to run on all instances | No |

All regular spawn flags work with job arrays:
- `--instance-type`, `--region`, `--az`
- `--ttl`, `--idle-timeout`, `--hibernate`
- `--spot`, `--dns`, `--user-data`
- `--key-name`, `--security-group`, `--subnet`

### Examples

```bash
# Simple 4-instance array
spawn launch --count 4 --job-array-name test --instance-type t3.micro

# With DNS (creates per-instance + group DNS)
spawn launch --count 8 --job-array-name compute \
  --instance-type m7i.large \
  --dns compute-cluster

# With custom names
spawn launch --count 5 --job-array-name training \
  --instance-names "worker-{index}" \
  --instance-type c7i.4xlarge

# With command (run on all instances)
spawn launch --count 10 --job-array-name sweep \
  --instance-type t3.small \
  --command "python train.py --param-id \$JOB_ARRAY_INDEX"

# Full example: MPI cluster with TTL
spawn launch --count 16 --job-array-name mpi-job \
  --instance-type c7i.8xlarge \
  --region us-east-1 \
  --ttl 4h \
  --user-data @setup-mpi.sh
```

---

## Coordination Metadata

### Environment Variables

Available **immediately** on instance startup in all shells:

```bash
JOB_ARRAY_ID        # Unique array ID: "compute-20260113-abc123"
JOB_ARRAY_NAME      # Human-readable name: "compute"
JOB_ARRAY_SIZE      # Total instances: "8"
JOB_ARRAY_INDEX     # This instance's index: "0" (0..N-1)
```

**Location**: Written to `/etc/profile.d/job-array.sh`

**Example usage:**
```bash
#!/bin/bash
echo "I am instance $JOB_ARRAY_INDEX of $JOB_ARRAY_SIZE"

# Leader node (rank 0)
if [ "$JOB_ARRAY_INDEX" -eq 0 ]; then
    echo "I am the leader"
fi

# Worker nodes
if [ "$JOB_ARRAY_INDEX" -gt 0 ]; then
    echo "I am worker $JOB_ARRAY_INDEX"
fi
```

### Peer Discovery File

Available **after all instances are running**: `/etc/spawn/job-array-peers.json`

**Format:**
```json
[
  {
    "index": 0,
    "instance_id": "i-abc123",
    "ip": "1.2.3.4",
    "dns": "compute-0.5k0zfnmq.spore.host"
  },
  {
    "index": 1,
    "instance_id": "i-def456",
    "ip": "1.2.3.5",
    "dns": "compute-1.5k0zfnmq.spore.host"
  }
]
```

**Barrier pattern** (wait for all peers):
```bash
# Block until peer file exists
while [ ! -f /etc/spawn/job-array-peers.json ]; do
    sleep 1
done

# Now safe to proceed - all instances are running
PEERS=$(cat /etc/spawn/job-array-peers.json)
```

---

## Usage Patterns

### Pattern 1: MPI Jobs (Coordinated)

**Use when**: Instances need to communicate (MPI, distributed training, etc.)

```bash
#!/bin/bash
# Wait for all peers (BARRIER)
while [ ! -f /etc/spawn/job-array-peers.json ]; do
    sleep 1
done

# Generate MPI hostfile
jq -r '.[] | .dns' /etc/spawn/job-array-peers.json > /tmp/hostfile

# Leader runs mpirun
if [ "$JOB_ARRAY_INDEX" -eq 0 ]; then
    mpirun -np $JOB_ARRAY_SIZE -hostfile /tmp/hostfile ./my-mpi-program
fi
```

**Full example:**
```bash
spawn launch --count 8 --job-array-name mpi-job \
  --instance-type c7i.4xlarge \
  --ttl 4h \
  --user-data @mpi-setup.sh
```

Where `mpi-setup.sh`:
```bash
#!/bin/bash
set -e

# Install MPI
sudo yum install -y openmpi openmpi-devel
export PATH=$PATH:/usr/lib64/openmpi/bin

# Wait for all peers
while [ ! -f /etc/spawn/job-array-peers.json ]; do sleep 1; done

# Setup SSH for passwordless communication
cat > ~/.ssh/config <<EOF
Host *
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
EOF

# Generate hostfile
jq -r '.[] | .dns' /etc/spawn/job-array-peers.json > /tmp/hostfile

# Compile MPI program
mpicc -o hello hello.c

# Run (leader only)
if [ "$JOB_ARRAY_INDEX" -eq 0 ]; then
    mpirun -np $JOB_ARRAY_SIZE -hostfile /tmp/hostfile ./hello
fi
```

### Pattern 2: Parameter Sweeps (Embarrassingly Parallel)

**Use when**: Instances work independently (no communication needed)

```bash
#!/bin/bash
# NO BARRIER - start immediately!

# Each instance processes its parameter
python train.py \
    --param-id $JOB_ARRAY_INDEX \
    --total-tasks $JOB_ARRAY_SIZE \
    --output results-${JOB_ARRAY_INDEX}.json
```

**Full example:**
```bash
spawn launch --count 100 --job-array-name hyperparam-sweep \
  --instance-type t3.small \
  --ttl 2h \
  --command "python sweep.py --task-id \$JOB_ARRAY_INDEX"
```

Where `sweep.py`:
```python
#!/usr/bin/env python3
import os
import json

# Get task assignment
task_id = int(os.environ['JOB_ARRAY_INDEX'])
total_tasks = int(os.environ['JOB_ARRAY_SIZE'])

# Load parameter grid
with open('params.json') as f:
    params = json.load(f)

# Process this task's parameters
my_params = params[task_id]
print(f"Task {task_id}/{total_tasks}: {my_params}")

# Run experiment
results = run_experiment(my_params)

# Save results
with open(f'results-{task_id}.json', 'w') as f:
    json.dump(results, f)
```

### Pattern 3: Leader-Worker

**Use when**: One coordinator and N workers

```bash
#!/bin/bash

if [ "$JOB_ARRAY_INDEX" -eq 0 ]; then
    # Leader: coordinate work
    echo "Starting coordinator..."
    python coordinator.py
else
    # Workers: wait for leader, then connect
    while [ ! -f /etc/spawn/job-array-peers.json ]; do sleep 1; done

    LEADER_IP=$(jq -r '.[] | select(.index==0) | .ip' /etc/spawn/job-array-peers.json)
    echo "Connecting to leader at $LEADER_IP"
    python worker.py --leader $LEADER_IP --rank $JOB_ARRAY_INDEX
fi
```

### Pattern 4: Distributed Training (PyTorch DDP)

```bash
#!/bin/bash
# Wait for all peers
while [ ! -f /etc/spawn/job-array-peers.json ]; do sleep 1; done

# Get leader address (rank 0)
MASTER_ADDR=$(jq -r '.[] | select(.index==0) | .ip' /etc/spawn/job-array-peers.json)
MASTER_PORT=29500

# Run distributed training
python -m torch.distributed.launch \
    --nproc_per_node=1 \
    --nnodes=$JOB_ARRAY_SIZE \
    --node_rank=$JOB_ARRAY_INDEX \
    --master_addr=$MASTER_ADDR \
    --master_port=$MASTER_PORT \
    train.py
```

---

## Management Commands

### List Job Arrays

```bash
# Show all instances (grouped by job array)
spawn list

# Filter by array name
spawn list --job-array-name compute

# Filter by array ID
spawn list --job-array-id compute-20260113-abc123
```

**Output:**
```
Job Arrays:

  compute (8 instances, 7 running, 1 pending)
  Array ID: compute-20260113-abc123
    [0] compute-0  i-abc123  m7i.large  running  us-east-1a  1.2.3.4
    [1] compute-1  i-def456  m7i.large  running  us-east-1b  1.2.3.5
    [2] compute-2  i-ghi789  m7i.large  pending  us-east-1c  -
    ...

Standalone Instances:
  my-instance  i-xyz123  t3.micro  running  us-east-1a  5.6.7.8
```

### Extend TTL

Extend TTL for all instances in array:

```bash
# By name
spawn extend --job-array-name compute 4h

# By ID
spawn extend --job-array-id compute-20260113-abc123 2h
```

Triggers configuration reload on all instances.

### Stop Instances

Stop all instances in array (preserves EBS, pauses TTL):

```bash
# By name
spawn stop --job-array-name compute

# By ID
spawn stop --job-array-id compute-20260113-abc123
```

### Hibernate Instances

Hibernate all instances (saves RAM to disk):

```bash
# By name
spawn hibernate --job-array-name compute

# By ID
spawn hibernate --job-array-id compute-20260113-abc123
```

### Start Instances

Start all stopped/hibernated instances:

```bash
# By name
spawn start --job-array-name compute

# By ID
spawn start --job-array-id compute-20260113-abc123
```

Restores remaining TTL automatically.

### Individual Instance Management

You can still manage individual instances:

```bash
# Extend single instance
spawn extend compute-0 2h

# Stop single instance
spawn stop i-abc123

# Connect to specific instance
spawn connect compute-0
```

---

## DNS Support

Job arrays support **dual DNS**:

### Per-Instance DNS

Each instance gets its own DNS name:

**Format**: `{name}-{index}.{account}.spore.host`

**Examples:**
```bash
# Instance 0
compute-0.5k0zfnmq.spore.host → 1.2.3.4

# Instance 1
compute-1.5k0zfnmq.spore.host → 1.2.3.5

# Instance 2
compute-2.5k0zfnmq.spore.host → 1.2.3.6
```

### Group DNS (Multi-Value)

Array also gets a group DNS name that resolves to **all** instance IPs:

**Format**: `{job-array-name}.{account}.spore.host`

**Example:**
```bash
$ dig +short compute.5k0zfnmq.spore.host
1.2.3.4
1.2.3.5
1.2.3.6
1.2.3.7
1.2.3.8
```

**Use cases:**
- Simple load balancing (DNS round-robin)
- Cluster discovery
- Health checking all instances

**Automatic updates:**
- When instance starts: IP added to group DNS
- When instance stops: IP removed from group DNS
- When last instance terminates: group DNS deleted

**Usage in scripts:**
```bash
# Get all peer IPs via DNS
dig +short compute.5k0zfnmq.spore.host > /tmp/peer-ips.txt

# Or use the peer file (recommended)
jq -r '.[] | .ip' /etc/spawn/job-array-peers.json > /tmp/peer-ips.txt
```

---

## Examples

### Example 1: Monte Carlo Simulation

**No barrier needed** - each instance runs independent trials:

```bash
spawn launch --count 50 --job-array-name monte-carlo \
  --instance-type c7i.large \
  --ttl 1h \
  --command "python simulate.py --trials 1000000 --seed \$JOB_ARRAY_INDEX"
```

Each instance runs 1M trials with a different random seed.

### Example 2: Video Rendering

**No barrier needed** - each instance renders different frames:

```bash
spawn launch --count 20 --job-array-name render \
  --instance-type g5.xlarge \
  --ttl 3h \
  --user-data @render.sh
```

Where `render.sh`:
```bash
#!/bin/bash
TOTAL_FRAMES=1000
FRAMES_PER_INSTANCE=$((TOTAL_FRAMES / JOB_ARRAY_SIZE))
START_FRAME=$((JOB_ARRAY_INDEX * FRAMES_PER_INSTANCE))
END_FRAME=$((START_FRAME + FRAMES_PER_INSTANCE - 1))

# Download scene
aws s3 cp s3://my-bucket/scene.blend /tmp/

# Render assigned frames
blender -b /tmp/scene.blend -s $START_FRAME -e $END_FRAME -a

# Upload results
aws s3 cp frames/ s3://my-bucket/output/ --recursive
```

### Example 3: Distributed Ray Cluster

**Barrier needed** - workers wait for head node:

```bash
spawn launch --count 8 --job-array-name ray-cluster \
  --instance-type m7i.2xlarge \
  --ttl 2h \
  --user-data @ray-setup.sh
```

Where `ray-setup.sh`:
```bash
#!/bin/bash
pip install ray[default]

# Wait for all peers
while [ ! -f /etc/spawn/job-array-peers.json ]; do sleep 1; done

if [ "$JOB_ARRAY_INDEX" -eq 0 ]; then
    # Head node
    ray start --head --port=6379

    # Run workload
    python my_ray_job.py
else
    # Worker nodes - connect to head
    HEAD_IP=$(jq -r '.[] | select(.index==0) | .ip' /etc/spawn/job-array-peers.json)
    ray start --address="${HEAD_IP}:6379"
fi
```

### Example 4: Genomics Pipeline

**No barrier needed** - process different chromosomes:

```bash
spawn launch --count 22 --job-array-name genomics \
  --instance-type r7i.4xlarge \
  --ttl 8h \
  --command "python process_chr.py --chr \$((JOB_ARRAY_INDEX + 1))"
```

Instance 0 processes chr1, instance 1 processes chr2, etc.

---

## Best Practices

### 1. Choose the Right Pattern

**Barrier (coordinated):**
- ✅ Use when instances communicate
- ✅ MPI, distributed training, clusters
- ❌ Slower startup (waits for all instances)

**No barrier (parallel):**
- ✅ Use when instances are independent
- ✅ Parameter sweeps, batch processing
- ✅ Faster startup (immediate execution)

### 2. Set Appropriate TTLs

```bash
# Short TTL for quick experiments
spawn launch --count 10 --job-array-name test --ttl 1h

# Longer TTL for production jobs
spawn launch --count 100 --job-array-name prod --ttl 12h

# No TTL for manual management
spawn launch --count 50 --job-array-name manual
```

### 3. Use Spot Instances for Cost Savings

```bash
spawn launch --count 50 --job-array-name batch \
  --instance-type c7i.large \
  --spot \
  --ttl 4h
```

**Note**: Spot interruptions affect individual instances, not the whole array.

### 4. Monitor with spawn list

```bash
# Check array status
spawn list --job-array-name compute

# Watch for completion
watch -n 5 'spawn list --job-array-name compute'
```

### 5. Use DNS for Persistence

Per-instance DNS survives stop/start cycles:

```bash
# Stop array for night
spawn stop --job-array-name compute

# Resume next day
spawn start --job-array-name compute

# DNS names unchanged
ssh ec2-user@compute-0.5k0zfnmq.spore.host
```

### 6. Error Handling in Scripts

```bash
#!/bin/bash
set -e  # Exit on error

# Timeout for peer file (30 seconds)
TIMEOUT=30
COUNT=0
while [ ! -f /etc/spawn/job-array-peers.json ]; do
    if [ $COUNT -ge $TIMEOUT ]; then
        echo "ERROR: Peer file not found after ${TIMEOUT}s"
        exit 1
    fi
    sleep 1
    COUNT=$((COUNT + 1))
done

# Validate peer file
if ! jq empty /etc/spawn/job-array-peers.json 2>/dev/null; then
    echo "ERROR: Invalid peer file JSON"
    exit 1
fi

# Continue with workload...
```

### 7. Resource Cleanup

Always set TTL or idle-timeout to prevent forgotten instances:

```bash
# TTL: terminate after 4 hours regardless
spawn launch --count 10 --job-array-name job --ttl 4h

# Idle timeout: terminate if CPU < 10% for 15 min
spawn launch --count 10 --job-array-name job --idle-timeout 15m

# Both: belt and suspenders
spawn launch --count 10 --job-array-name job --ttl 8h --idle-timeout 30m
```

### 8. Debugging

```bash
# SSH to specific instance
spawn connect compute-0

# Check environment
env | grep JOB_ARRAY

# Check peer file
cat /etc/spawn/job-array-peers.json | jq

# Check logs
sudo journalctl -u spored -f

# Check user-data execution
sudo cat /var/log/cloud-init-output.log
```

---

## Advanced Topics

### Cross-Region Job Arrays

Currently job arrays run in a single region. For multi-region:

```bash
# Launch separate arrays per region
spawn launch --count 5 --job-array-name us-east --region us-east-1
spawn launch --count 5 --job-array-name us-west --region us-west-2

# Coordinate via external service (S3, DynamoDB, etc.)
```

### Dynamic Scaling

Add more instances to a running array:

```bash
# Get existing array ID
ARRAY_ID=$(spawn list --job-array-name compute -o json | jq -r '.[0].job_array_id')

# Launch additional instances with same array ID
# TODO: Not yet implemented - would require manual tag setting
```

### Failure Handling

Instance failures don't affect the array:

```bash
# Check for failed instances
spawn list --job-array-name compute --state terminated

# Replace failed instance
spawn launch --instance-type m7i.large --name compute-5-replacement
```

### Job Array Chains

Run dependent job arrays:

```bash
# Stage 1: Data preprocessing
spawn launch --count 10 --job-array-name preprocess --ttl 2h

# Wait for completion (manual or scripted)
# ...

# Stage 2: Model training (uses preprocessed data)
spawn launch --count 5 --job-array-name training --ttl 4h
```

---

## Tags

All instances in a job array have these tags:

```
spawn:managed          = "true"
spawn:job-array-id     = "compute-20260113-abc123"  # Unique ID
spawn:job-array-name   = "compute"                  # User-friendly name
spawn:job-array-index  = "0"                        # Instance index (0..N-1)
spawn:job-array-size   = "8"                        # Total instances
spawn:job-array-peers  = "[{...}]"                  # Peer info JSON
```

Plus all standard spawn tags (ttl, idle-timeout, etc.).

Use for custom automation:

```bash
# Find all instances in array
aws ec2 describe-instances \
  --filters "Name=tag:spawn:job-array-id,Values=compute-20260113-abc123"

# Custom CloudWatch alarms per array
aws cloudwatch put-metric-alarm \
  --alarm-name "compute-array-health" \
  --dimensions Name=JobArrayID,Value=compute-20260113-abc123
```

---

## Troubleshooting

### Peer File Not Appearing

**Symptom**: `/etc/spawn/job-array-peers.json` doesn't exist

**Causes:**
1. Not all instances finished launching yet
2. spored agent hasn't started
3. Instance has no job array tags

**Debug:**
```bash
# Check if instance has job array tags
aws ec2 describe-tags --filters "Name=resource-id,Values=$(ec2-metadata --instance-id | cut -d' ' -f2)"

# Check spored status
sudo systemctl status spored

# Check spored logs
sudo journalctl -u spored -n 50
```

### Instances Not in Same Array

**Symptom**: Some instances missing from peer file

**Causes:**
1. Instances launched separately (different job-array-id)
2. Some instances failed to launch

**Debug:**
```bash
# Check array IDs match
spawn list --job-array-name compute

# Look for launch failures in spawn output
```

### DNS Not Resolving

**Symptom**: Group DNS doesn't return IPs

**Causes:**
1. DNS registration failed
2. Wrong account subdomain
3. Lambda function issue

**Debug:**
```bash
# Check per-instance DNS first
dig +short compute-0.5k0zfnmq.spore.host

# Check spored DNS registration logs
sudo journalctl -u spored | grep -i dns

# Verify account base36
python3 -c "
def base36_encode(n):
    alphabet = '0123456789abcdefghijklmnopqrstuvwxyz'
    result = []
    while n:
        n, r = divmod(n, 36)
        result.append(alphabet[r])
    return ''.join(reversed(result))
import boto3
account = boto3.client('sts').get_caller_identity()['Account']
print(f'{account} -> {base36_encode(int(account))}')
"
```

---

## Limits

- **Max instances per array**: No hard limit, AWS account limits apply
- **Max arrays per account**: No hard limit
- **Peer file size**: ~1KB per 10 instances (negligible)
- **Launch parallelism**: Launches all instances simultaneously

AWS EC2 default limits:
- On-Demand instances: Varies by instance type
- Spot instances: Varies by instance type
- See: `truffle quota` to check your limits

---

## Migration from Manual Coordination

If you're currently managing instance groups manually:

**Before:**
```bash
# Manually launch and track instances
for i in {0..7}; do
  aws ec2 run-instances --tag-specifications "ResourceType=instance,Tags=[{Key=Index,Value=$i}]"
done

# Manually collect IPs
aws ec2 describe-instances --filters ... | jq ...

# Manually create hostfiles
echo "1.2.3.4" >> hostfile
```

**After:**
```bash
# One command
spawn launch --count 8 --job-array-name compute

# Peer file auto-created
cat /etc/spawn/job-array-peers.json
```

---

## See Also

- [README.md](README.md) - Main spawn documentation
- [DNS_SETUP.md](DNS_SETUP.md) - DNS configuration details
- [IAM_PERMISSIONS.md](IAM_PERMISSIONS.md) - Required AWS permissions
- [MONITORING.md](MONITORING.md) - Monitoring and observability

---

## Questions?

File an issue: https://github.com/yourusername/spawn/issues
