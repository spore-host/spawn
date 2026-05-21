# How-To: Slurm Integration

Convert Slurm workloads to spawn or run spawn alongside Slurm.

## Overview

### Slurm vs spawn

| Feature | Slurm | spawn |
|---------|-------|-------|
| **Cluster** | Fixed capacity | Dynamic (AWS) |
| **Scaling** | Manual | Automatic |
| **Cost** | Fixed | Pay-per-use |
| **Setup** | Complex | Simple |
| **Job submission** | `sbatch` | `spawn launch` |
| **Arrays** | `--array=1-100` | `--array 100` |
| **Dependencies** | `--dependency` | Batch queue |

### When to Use Each

**Use Slurm when:**
- Existing on-premises cluster
- Bare-metal performance required
- Complex scheduler policies needed

**Use spawn when:**
- Cloud-first workloads
- Burst capacity needed
- Variable workload patterns
- No cluster maintenance desired

**Use both:**
- Hybrid: Slurm on-prem + spawn for burst
- Migration: Gradual transition from Slurm to spawn

---

## Converting sbatch Scripts

### Simple Job Conversion

**Slurm:**
```bash
#!/bin/bash
#SBATCH --job-name=myjob
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=4
#SBATCH --mem=16G
#SBATCH --time=02:00:00
#SBATCH --output=job-%j.out

module load python/3.9
python train.py
```

**spawn equivalent:**
```bash
#!/bin/bash
# No need for sbatch directives

spawn launch \
  --instance-type c7i.xlarge \
  --ttl 2h \
  --name myjob \
  --on-complete terminate \
  --user-data "
    python train.py > job-\${INSTANCE_ID}.out 2>&1
    aws s3 cp job-\${INSTANCE_ID}.out s3://logs/job-\${INSTANCE_ID}.out
    spored complete --status success
  "
```

**Instance type mapping:**
```bash
# Slurm: --cpus-per-task=4 --mem=16G
# spawn: c7i.xlarge (4 vCPU, 8 GB) or m7i.xlarge (4 vCPU, 16 GB)

# Slurm: --cpus-per-task=8 --mem=32G
# spawn: c7i.2xlarge (8 vCPU, 16 GB) or m7i.2xlarge (8 vCPU, 32 GB)

# Slurm: --gres=gpu:1 --mem=32G
# spawn: g5.xlarge (4 vCPU, 16 GB, 1 GPU)

# Slurm: --gres=gpu:4 --mem=192G
# spawn: g5.12xlarge (48 vCPU, 192 GB, 4 GPUs)
```

---

## Job Arrays

### Slurm Array Job

**Slurm:**
```bash
#!/bin/bash
#SBATCH --job-name=array-job
#SBATCH --array=1-100
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=4
#SBATCH --time=01:00:00
#SBATCH --output=job-%A-%a.out

# Process task based on SLURM_ARRAY_TASK_ID
python process.py --task-id $SLURM_ARRAY_TASK_ID
```

**spawn equivalent:**
```bash
spawn launch \
  --instance-type c7i.xlarge \
  --array 100 \
  --ttl 1h \
  --name array-job \
  --on-complete terminate \
  --user-data "
    # TASK_ARRAY_INDEX is 0-based (Slurm is 1-based)
    TASK_ID=\$((TASK_ARRAY_INDEX + 1))

    python process.py --task-id \$TASK_ID > job-\${TASK_ARRAY_INDEX}.out 2>&1

    # Upload output
    aws s3 cp job-\${TASK_ARRAY_INDEX}.out s3://logs/job-\${TASK_ARRAY_INDEX}.out

    spored complete --status success
  "
```

**Array with step size:**

**Slurm:**
```bash
#SBATCH --array=0-100:10
# Runs: 0, 10, 20, 30, ..., 100
```

**spawn equivalent:**
```bash
# Launch 11 instances (0-10)
spawn launch --array 11 --user-data "
  TASK_ID=\$((TASK_ARRAY_INDEX * 10))
  python process.py --task-id \$TASK_ID
"
```

---

## Job Dependencies

### Simple Dependency

**Slurm:**
```bash
# Job 1
JOB1=$(sbatch --parsable job1.sh)

# Job 2 runs after Job 1 completes
sbatch --dependency=afterok:$JOB1 job2.sh
```

**spawn equivalent using batch queue:**
```json
{
  "jobs": [
    {
      "job_id": "job1",
      "instance_type": "c7i.xlarge",
      "ttl": "1h",
      "command": "python job1.py"
    },
    {
      "job_id": "job2",
      "instance_type": "c7i.xlarge",
      "ttl": "1h",
      "command": "python job2.py",
      "depends_on": ["job1"]
    }
  ]
}
```

```bash
spawn queue create my-queue --config queue.json
spawn queue start my-queue
```

---

## Resource Constraints

### Memory Constraints

**Slurm:**
```bash
#SBATCH --mem=64G
#SBATCH --mem-per-cpu=16G
```

**spawn equivalent:**
```bash
# Select instance type with required memory
spawn launch --instance-type r7i.2xlarge  # 64 GB memory
spawn launch --instance-type m7i.4xlarge  # 64 GB memory
```

### GPU Constraints

**Slurm:**
```bash
#SBATCH --gres=gpu:v100:2
#SBATCH --gres=gpu:a100:1
```

**spawn equivalent:**
```bash
# V100-equivalent: g5 instances
spawn launch --instance-type g5.2xlarge  # 1 GPU (24 GB)

# A100-equivalent: p4d instances
spawn launch --instance-type p4d.24xlarge  # 8 GPUs (40 GB each)

# Or filter by GPU type
spawn launch --instance-type g5.xlarge --ami ami-gpu-cuda-11
```

---

## Partition/Queue Mapping

### Slurm Partitions

**Slurm:**
```bash
#SBATCH --partition=short    # 1 hour limit
#SBATCH --partition=medium   # 24 hour limit
#SBATCH --partition=long     # 7 day limit
#SBATCH --partition=gpu      # GPU nodes
```

**spawn equivalent:**
```bash
# Use TTL for time limits
spawn launch --ttl 1h ...    # "short"
spawn launch --ttl 24h ...   # "medium"
spawn launch --ttl 7d ...    # "long"

# Use instance type for resources
spawn launch --instance-type g5.xlarge ...  # "gpu"
spawn launch --instance-type c7i.xlarge ... # "cpu"

# Or use tags for organization
spawn launch --tags partition=short ...
spawn launch --tags partition=gpu ...
```

---

## Environment Modules

### Slurm Modules

**Slurm:**
```bash
#!/bin/bash
#SBATCH --job-name=train

module load python/3.9
module load cuda/11.8
module load pytorch/2.0

python train.py
```

**spawn equivalent (using custom AMI):**
```bash
# Option 1: Custom AMI with pre-installed software
spawn launch \
  --ami ami-ml-pytorch \
  --instance-type g5.xlarge \
  --user-data "python train.py"

# Option 2: Install in user-data
spawn launch \
  --instance-type g5.xlarge \
  --user-data "
    # Setup environment
    conda activate pytorch-env
    python train.py
  "

# Option 3: Use container
spawn launch \
  --instance-type g5.xlarge \
  --user-data "
    docker run --gpus all pytorch/pytorch:2.0-cuda11.8 python train.py
  "
```

---

## Slurm Commands Cheat Sheet

### Job Submission

| Slurm | spawn |
|-------|-------|
| `sbatch job.sh` | `spawn launch --user-data @job.sh` |
| `srun command` | `spawn launch --user-data "command"` |
| `salloc -N 1` | `spawn launch --wait-for-ssh` |

### Job Management

| Slurm | spawn |
|-------|-------|
| `squeue` | `spawn list` |
| `squeue -u $USER` | `spawn list --tag owner=$USER` |
| `scancel <job-id>` | `spawn cancel <instance-id>` |
| `scancel -u $USER` | `spawn cancel --tag owner=$USER` |
| `scontrol show job <id>` | `spawn status <instance-id>` |

### Job Arrays

| Slurm | spawn |
|-------|-------|
| `--array=1-100` | `--array 100` |
| `$SLURM_ARRAY_TASK_ID` | `$TASK_ARRAY_INDEX` (0-based) |
| `$SLURM_ARRAY_JOB_ID` | N/A (use tags) |

### Resources

| Slurm | spawn |
|-------|-------|
| `--cpus-per-task=8` | `--instance-type c7i.2xlarge` |
| `--mem=32G` | `--instance-type m7i.2xlarge` |
| `--gres=gpu:1` | `--instance-type g5.xlarge` |
| `--time=02:00:00` | `--ttl 2h` |

---

## Hybrid Slurm + spawn

### Problem
Existing Slurm cluster is full, need burst capacity.

### Solution: Offload overflow to spawn

**Slurm submission wrapper:**
```bash
#!/bin/bash
# smart-submit.sh - Submit to Slurm or spawn based on queue

# Check Slurm queue depth
PENDING=$(squeue -t PENDING -u $USER | wc -l)

if [ $PENDING -lt 10 ]; then
  # Queue not full, use Slurm
  echo "Submitting to Slurm..."
  sbatch "$@"
else
  # Queue full, use spawn
  echo "Slurm queue full, using spawn..."

  # Parse sbatch script to extract spawn parameters
  SCRIPT=$1
  CPUS=$(grep "#SBATCH --cpus-per-task" $SCRIPT | awk '{print $NF}')
  MEM=$(grep "#SBATCH --mem" $SCRIPT | awk '{print $NF}')
  TIME=$(grep "#SBATCH --time" $SCRIPT | awk '{print $NF}')

  # Map to instance type
  if [ $CPUS -le 4 ]; then
    INSTANCE_TYPE="c7i.xlarge"
  elif [ $CPUS -le 8 ]; then
    INSTANCE_TYPE="c7i.2xlarge"
  else
    INSTANCE_TYPE="c7i.4xlarge"
  fi

  # Extract script content (skip SBATCH directives)
  SCRIPT_CONTENT=$(grep -v "^#SBATCH" $SCRIPT)

  # Launch on spawn
  spawn launch \
    --instance-type $INSTANCE_TYPE \
    --ttl $TIME \
    --tags source=slurm-overflow,user=$USER \
    --on-complete terminate \
    --user-data "$SCRIPT_CONTENT"
fi
```

---

## Migration Strategy

### Phase 1: Parallel Running

**Run same job on both:**
```bash
#!/bin/bash
# run-on-both.sh

JOB_SCRIPT=$1

# Submit to Slurm
SLURM_JOB=$(sbatch --parsable $JOB_SCRIPT)
echo "Slurm job: $SLURM_JOB"

# Submit to spawn
SPAWN_INSTANCE=$(spawn launch \
  --instance-type c7i.xlarge \
  --ttl 2h \
  --user-data @$JOB_SCRIPT \
  --quiet)
echo "spawn instance: $SPAWN_INSTANCE"

# Compare results later
echo "Compare results:"
echo "  Slurm: slurm-$SLURM_JOB.out"
echo "  spawn: s3://results/spawn-$SPAWN_INSTANCE.out"
```

### Phase 2: Selective Migration

**Migrate by workload type:**
```bash
# Keep CPU-intensive on Slurm (cheaper on owned hardware)
sbatch cpu-intensive-job.sh

# Move GPU workloads to spawn (no GPU cluster)
spawn launch --instance-type g5.xlarge --user-data @gpu-job.sh

# Move burst workloads to spawn
spawn launch --array 1000 --spot --user-data @burst-job.sh
```

### Phase 3: Full Migration

**Wrapper to make spawn look like Slurm:**
```bash
#!/bin/bash
# sbatch-compat.sh - Drop-in replacement for sbatch

SCRIPT=$1

# Parse sbatch directives
JOB_NAME=$(grep "#SBATCH --job-name" $SCRIPT | awk -F= '{print $2}')
CPUS=$(grep "#SBATCH --cpus-per-task" $SCRIPT | awk '{print $NF}')
MEM=$(grep "#SBATCH --mem" $SCRIPT | awk '{print $NF}' | tr -d 'G')
TIME=$(grep "#SBATCH --time" $SCRIPT | awk '{print $NF}')
ARRAY=$(grep "#SBATCH --array" $SCRIPT | awk -F= '{print $2}')

# Map resources to instance type
if [ -n "$MEM" ] && [ $MEM -ge 64 ]; then
  INSTANCE_TYPE="r7i.2xlarge"
elif [ -n "$CPUS" ] && [ $CPUS -ge 16 ]; then
  INSTANCE_TYPE="c7i.4xlarge"
elif [ -n "$CPUS" ] && [ $CPUS -ge 8 ]; then
  INSTANCE_TYPE="c7i.2xlarge"
else
  INSTANCE_TYPE="c7i.xlarge"
fi

# Check for GPU
if grep -q "#SBATCH --gres=gpu" $SCRIPT; then
  INSTANCE_TYPE="g5.xlarge"
fi

# Parse time (HH:MM:SS → hours)
IFS=: read -r hours minutes seconds <<< "$TIME"
TTL="${hours}h"

# Extract script content
SCRIPT_CONTENT=$(grep -v "^#SBATCH" $SCRIPT | grep -v "^#")

# Build spawn command
CMD="spawn launch --instance-type $INSTANCE_TYPE --ttl $TTL"

if [ -n "$JOB_NAME" ]; then
  CMD="$CMD --name $JOB_NAME"
fi

if [ -n "$ARRAY" ]; then
  # Parse array spec (e.g., "1-100")
  ARRAY_SIZE=$(echo $ARRAY | cut -d- -f2)
  CMD="$CMD --array $ARRAY_SIZE"
fi

CMD="$CMD --on-complete terminate --user-data \"$SCRIPT_CONTENT\""

# Execute
eval $CMD
```

**Usage:**
```bash
# Replace sbatch with sbatch-compat.sh
alias sbatch=/usr/local/bin/sbatch-compat.sh

# Existing scripts work unchanged
sbatch myjob.sh
```

---

## Advanced Patterns

### MPI Jobs

**Slurm:**
```bash
#!/bin/bash
#SBATCH --ntasks=16
#SBATCH --ntasks-per-node=4

mpirun -np $SLURM_NTASKS ./simulation
```

**spawn equivalent:**
```bash
spawn launch \
  --instance-type c7i.xlarge \
  --cluster-size 4 \
  --ttl 2h \
  --mpi-command "./simulation"
```

### Checkpoint/Restart

**Slurm:**
```bash
#!/bin/bash
#SBATCH --requeue
#SBATCH --signal=TERM@120

# Checkpoint handler
trap 'checkpoint_and_exit' TERM

checkpoint_and_exit() {
  save_checkpoint
  exit 0
}

# Main loop
while not_done; do
  compute_step
  periodic_checkpoint
done
```

**spawn equivalent:**
```bash
spawn launch \
  --instance-type c7i.xlarge \
  --spot \
  --ttl 4h \
  --user-data "
    # Checkpoint handler for spot interruption
    trap 'checkpoint_and_exit' SIGTERM

    checkpoint_and_exit() {
      echo 'Spot interruption, checkpointing...'
      python save_checkpoint.py
      aws s3 cp checkpoint.pkl s3://checkpoints/latest.pkl
      exit 0
    }

    # Resume from checkpoint if exists
    if aws s3 ls s3://checkpoints/latest.pkl; then
      aws s3 cp s3://checkpoints/latest.pkl checkpoint.pkl
      python resume_from_checkpoint.py checkpoint.pkl
    else
      python train_from_scratch.py
    fi
  "
```

---

## Cost Comparison

### Slurm (On-Premises)

**Costs:**
- Hardware: $50k per node (amortized over 3 years)
- Power: $100/month per node
- Cooling: $50/month per node
- Maintenance: Staff time

**10-node cluster:**
- Upfront: $500k
- Monthly: $1,500
- 3-year TCO: $554k

### spawn (Cloud)

**Costs (on-demand):**
- c7i.xlarge: $0.17/hour
- 10 instances × 24/7: $1,224/month
- 3-year cost: $44,064

**Costs (spot):**
- c7i.xlarge spot: ~$0.05/hour (70% savings)
- 10 instances × 24/7: $367/month
- 3-year cost: $13,212

**Typical usage (8 hours/day, 5 days/week):**
- On-demand: $544/month = $19,584/3-year
- Spot: $163/month = $5,868/3-year

**Verdict:** spawn is 28x cheaper for typical burst workloads.

---

## Troubleshooting

### Job Doesn't Start

**Slurm:** Check `squeue`, cluster may be full.
**spawn:** Check AWS quotas, may need limit increase.

```bash
# Check spawn quotas
aws service-quotas get-service-quota \
  --service-code ec2 \
  --quota-code L-1216C47A  # On-Demand instances

# Request increase
aws service-quotas request-service-quota-increase \
  --service-code ec2 \
  --quota-code L-1216C47A \
  --desired-value 100
```

### Job Timeout

**Slurm:** Increase `--time`.
**spawn:** Increase `--ttl` or use idle timeout.

```bash
# Extend TTL for running instance
spawn extend <instance-id> --ttl 2h
```

### Out of Memory

**Slurm:** Increase `--mem`.
**spawn:** Use larger instance type.

```bash
# Launch with more memory
spawn launch --instance-type r7i.2xlarge  # 64 GB
```

---

## See Also

- [Tutorial 4: Job Arrays](../tutorials/04-job-arrays.md) - Array basics
- [How-To: Batch Queues](batch-queues.md) - Job dependencies
- [How-To: Cost Optimization](cost-optimization.md) - Reduce costs
- [spawn launch](../reference/commands/launch.md) - Launch reference
- [Slurm Documentation](https://slurm.schedmd.com/)
