# spawn Examples

This directory contains example configurations for common spawn use cases.

## Directory Structure

- **`slurm/`** - Slurm batch script examples for HPC migration
- **`staging/`** - Data staging examples for multi-region distribution
- **`sweeps/`** - Parameter sweep examples for hyperparameter tuning
- **`mpi/`** - MPI job examples for parallel computing
- **Scheduled Executions** - Parameter files for scheduled sweeps (e.g., `schedule-params.yaml`)
- **Batch Queues** - Sequential job queue configurations (e.g., `ml-pipeline-queue.json`, `simple-queue.json`)

## Quick Start

### Slurm Migration

1. **Convert existing Slurm script:**
```bash
spawn slurm convert examples/slurm/array-cpu.sbatch --output params.yaml
```

2. **Estimate cost:**
```bash
spawn slurm estimate examples/slurm/gpu-training.sbatch
```

3. **Submit job:**
```bash
spawn slurm submit examples/slurm/bioinformatics.sbatch --spot
```

### Data Staging

1. **Stage large dataset:**
```bash
# Example: 100GB reference genome
spawn stage upload hg38-reference.fasta \
  --regions us-east-1,us-west-2 \
  --dest /mnt/data/reference.fasta
```

2. **Launch with staged data:**
```bash
spawn launch --params examples/staging/reference-genome.yaml
```

3. **Cost savings:**
   - Without staging: $900 (100 instances × 100GB × $0.09/GB)
   - With staging: $2 (replication cost)
   - **Savings: 99.8%**

### Parameter Sweeps

1. **Launch hyperparameter search:**
```bash
spawn launch --params examples/sweeps/hyperparameter-search.yaml
```

2. **Monitor progress:**
```bash
spawn sweep status <sweep-id>
```

3. **Collect top results:**
```bash
spawn collect-results --sweep-id <sweep-id> \
  --metric accuracy \
  --best 5 \
  --output top5.csv
```

### MPI Jobs

1. **Launch MPI simulation:**
```bash
spawn launch --params examples/mpi/mpi-simulation.yaml --mpi
```

2. **Features:**
   - Automatic placement groups
   - EFA for ultra-low latency
   - Passwordless SSH between nodes
   - Automatic hostfile generation

### Scheduled Executions

1. **Schedule nightly training:**
```bash
spawn schedule create schedule-params.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York" \
  --name "nightly-training"
```

2. **One-time scheduled execution:**
```bash
spawn schedule create schedule-params.yaml \
  --at "2026-01-24T15:00:00" \
  --timezone "America/New_York"
```

3. **Monitor schedules:**
```bash
# List all schedules
spawn schedule list

# View details and execution history
spawn schedule describe <schedule-id>

# Pause/resume
spawn schedule pause <schedule-id>
spawn schedule resume <schedule-id>
```

### Batch Job Queues

1. **Launch sequential job pipeline:**
```bash
spawn launch --batch-queue ml-pipeline-queue.json --instance-type g5.2xlarge
```

2. **Simple batch processing:**
```bash
spawn launch --batch-queue simple-queue.json --instance-type t3.medium
```

3. **Monitor queue execution:**
```bash
# Check status
spawn queue status <instance-id>

# Download results
spawn queue results <queue-id> --output ./results/
```

4. **Features:**
   - Sequential job execution with dependencies
   - Automatic retry with exponential backoff
   - Per-job result collection
   - State persistence and resume capability
   - Environment variable injection per job

## Example Descriptions

### Slurm Scripts

**`array-cpu.sbatch`**
- 100-task array job
- 4 CPUs, 8GB RAM per task
- 2-hour runtime
- Perfect for parameter sweeps

**`gpu-training.sbatch`**
- 2× V100 GPUs
- 64GB RAM, 16 CPUs
- 8-hour training
- PyTorch/TensorFlow ready

**`bioinformatics.sbatch`**
- 50-sample genome analysis
- BWA/samtools pipeline
- Reference genome staging
- 4-hour per sample

### Staging Examples

**`ml-training.yaml`**
- 200GB training dataset
- 30 instances across 3 regions
- $1,072 cost savings (99.3%)
- Distributed PyTorch training

**`reference-genome.yaml`**
- 100GB reference genome (hg38)
- 100 sample analysis
- $898 cost savings (99.8%)
- BWA alignment pipeline

### Sweep Examples

**`hyperparameter-search.yaml`**
- 48-trial grid search
- 4 learning rates × 3 batch sizes × 4 models
- Multi-region distribution
- Automatic result collection

### MPI Examples

**`mpi-simulation.yaml`**
- 16-node MPI job (576 processes)
- EFA-enabled for < 10μs latency
- Placement groups for co-location
- 24-hour simulation

### Scheduled Execution Examples

**`schedule-params.yaml`**
- Nightly training runs
- 11 configurations (learning rates, batch sizes, models, optimizers)
- Automatic execution at 2 AM
- Max 30 days of executions
- Typical use: Continuous experimentation without manual intervention

**`simple-params.yaml`**
- Basic 3-configuration sweep
- Learning rate comparison
- Quick start for scheduling

### Batch Queue Examples

**`ml-pipeline-queue.json`**
- Production ML pipeline
- 4 sequential jobs: preprocess → train → evaluate → export
- Per-job retry with exponential backoff
- Environment variables for GPU config
- Automatic result collection
- Total runtime: ~3 hours

**`simple-queue.json`**
- Basic 3-step pipeline
- Setup → Process → Cleanup
- Result path collection
- Perfect for learning batch queues

## Cost Comparison

| Use Case | Cluster (Free) | spawn On-Demand | spawn Spot | Savings |
|----------|----------------|-----------------|------------|---------|
| 100-task array | 2-3 days queue | $332 (2h) | $100 (2h) | Time > Cost |
| GPU training | 1 week queue | $48 (8h) | $14 (8h) | Time > Cost |
| Genome analysis | 3 days queue | $440 (4h) | $132 (4h) | Time > Cost |
| MPI simulation | 2 weeks queue | $1,152 (24h) | N/A | Time > Cost |

## Best Practices

### When to Use Spot Instances

✅ **Use spot:**
- Array jobs (independent tasks)
- Short-duration tasks (< 2 hours)
- Fault-tolerant workloads
- Development/testing

❌ **Avoid spot:**
- Long-running tasks (> 4 hours)
- Tightly-coupled MPI (> 16 nodes)
- Time-critical deadlines
- Stateful applications

### When to Use Data Staging

✅ **Use staging when:**
- Dataset > 10GB
- Multiple regions
- > 5 instances per region
- Data doesn't change

**Break-even:** Just 1 instance per region!

### Instance Type Selection

| Workload | Instance Family | Example |
|----------|----------------|---------|
| CPU-intensive | C5, C6i | c5.4xlarge |
| Memory-intensive | R5, R6i | r5.2xlarge |
| GPU training | P3, P4d | p3.2xlarge |
| GPU inference | G4dn, G5 | g4dn.xlarge |
| MPI/HPC | C5n, C6gn | c5n.18xlarge |

### When to Use Scheduled Executions

✅ **Use schedules for:**
- Nightly training runs with fresh data
- Weekly model retraining
- Periodic batch processing
- Continuous experimentation without manual intervention
- Time-based triggers (e.g., after data refresh)

**Tips:**
- Always specify timezone to handle DST transitions
- Use `max_executions` to limit total runs
- Set `end_after` date for time-limited experiments
- Monitor execution history regularly

### When to Use Batch Queues

✅ **Use batch queues for:**
- Multi-step ML pipelines (preprocess → train → evaluate)
- Sequential ETL workflows
- CI/CD pipelines with stages
- Jobs with dependencies between steps

**Tips:**
- Use `on_failure: "stop"` for critical pipelines
- Use `on_failure: "continue"` for independent steps
- Configure retry with exponential backoff for transient errors
- Use environment variables for per-job configuration
- Specify result paths to collect outputs incrementally

## Getting Help

- **Documentation:**
  - [SLURM_GUIDE.md](../SLURM_GUIDE.md) - Slurm migration
  - [DATA_STAGING_GUIDE.md](../DATA_STAGING_GUIDE.md) - Multi-region data staging
  - [SCHEDULED_EXECUTIONS_GUIDE.md](../SCHEDULED_EXECUTIONS_GUIDE.md) - Scheduled sweeps
  - [BATCH_QUEUE_GUIDE.md](../BATCH_QUEUE_GUIDE.md) - Sequential job queues
- **Troubleshooting:** See [TROUBLESHOOTING.md](../TROUBLESHOOTING.md)
- **Issues:** https://github.com/spore-host/spawn/issues

## Contributing Examples

Have a useful example? Submit a PR!

**Example template:**
```yaml
# Clear description of use case
sweep_name: my-example
count: 10
regions: [us-east-1]
instance_type: t3.medium

base_command: |
  #!/bin/bash
  # Well-commented commands
  echo "Example task"
```
