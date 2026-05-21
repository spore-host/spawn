# Slurm Integration Guide

**Table of Contents**
- [Introduction](#introduction)
- [Quick Start](#quick-start)
- [Command Reference](#command-reference)
- [Slurm Directive Support](#slurm-directive-support)
- [Job Type Examples](#job-type-examples)
- [Migration Workflow](#migration-workflow)
- [Cost Comparison](#cost-comparison)
- [Advanced Topics](#advanced-topics)
- [Troubleshooting](#troubleshooting)
- [Examples](#examples)

---

## Introduction

### What is Slurm Integration?

Spawn's Slurm integration enables HPC researchers to run existing Slurm batch scripts on AWS without modification. The `spawn slurm` commands parse Slurm directives (`#SBATCH`) and convert them to equivalent spawn parameter sweeps.

**Key Benefits:**
- **Zero code changes**: Run existing `.sbatch` scripts directly
- **No queue wait times**: Launch immediately on AWS
- **Cost-effective**: Use spot instances for 70-90% savings
- **Elastic capacity**: Scale from 1 to 10,000+ instances
- **Familiar workflow**: Keep using Slurm syntax you know

### Who Should Use This?

- HPC users migrating workloads to the cloud
- Researchers needing faster turnaround than institutional clusters
- Teams running embarrassingly parallel workloads
- Users with bursty compute needs (month-end, deadlines)
- Anyone with existing Slurm scripts

### When to Use Spawn vs. Institutional Cluster

**Use Spawn When:**
- You need results immediately (no queue wait)
- Running array jobs with 100+ independent tasks
- Short-duration jobs (< 8 hours)
- Development/testing workflows
- Deadline-driven work

**Use Institutional Cluster When:**
- Long-running jobs (> 24 hours)
- Tightly-coupled MPI with 100+ nodes
- Budget constraints (cluster is "free")
- Using cluster-specific resources (special hardware, licensed software)

---

## Quick Start

### Installation

Ensure you have spawn installed:

```bash
# Download latest release
curl -L https://github.com/spore-host/spore-host/releases/latest/download/spawn-$(uname -s)-$(uname -m) -o spawn
chmod +x spawn
sudo mv spawn /usr/local/bin/
```

### Your First Slurm Conversion

1. **Create a Slurm script** (`my_job.sbatch`):

```bash
#!/bin/bash
#SBATCH --job-name=test-array
#SBATCH --array=1-10
#SBATCH --time=01:00:00
#SBATCH --mem=4G
#SBATCH --cpus-per-task=2

echo "Running task ${SLURM_ARRAY_TASK_ID}"
./my_program $SLURM_ARRAY_TASK_ID
```

2. **Convert to spawn parameters:**

```bash
spawn slurm convert my_job.sbatch --output params.yaml
```

Output (`params.yaml`):
```yaml
sweep_name: test-array
base_command: |
  #!/bin/bash
  echo "Running task ${SWEEP_INDEX}"
  ./my_program $SWEEP_INDEX
instance_type: t3.medium  # Auto-selected based on 2 CPUs, 4GB RAM
ttl: 1h
count: 10
regions:
  - us-east-1
```

3. **Review and launch:**

```bash
# Review the conversion
cat params.yaml

# Launch the sweep
spawn launch --params params.yaml
```

4. **Or combine convert + launch in one step:**

```bash
spawn slurm submit my_job.sbatch --spot --yes
```

---

## Command Reference

### `spawn slurm convert`

Converts a Slurm batch script to spawn parameter format.

```bash
spawn slurm convert <script.sbatch> [--output FILE]
```

**Arguments:**
- `<script.sbatch>`: Path to Slurm batch script

**Flags:**
- `--output`, `-o`: Output file (default: stdout)

**Examples:**
```bash
# Convert to stdout
spawn slurm convert job.sbatch

# Convert to file
spawn slurm convert job.sbatch --output params.yaml

# Review and edit before launching
spawn slurm convert job.sbatch -o params.yaml
vim params.yaml
spawn launch --params params.yaml
```

---

### `spawn slurm estimate`

Estimates the cost of running a Slurm script on spawn.

```bash
spawn slurm estimate <script.sbatch>
```

**Output:**
```
Slurm Script: job.sbatch
Job Name: test-array
Array Size: 100 tasks
Duration: 2h per task
Memory: 8GB
CPUs: 4

Instance Selection:
  Recommended: t3.xlarge (4 vCPUs, 16GB RAM)
  On-Demand: $0.1664/hour
  Spot: ~$0.05/hour (70% savings)

Cost Estimate:
  On-Demand: 100 tasks × 2h × $0.17 = $34.00
  Spot:      100 tasks × 2h × $0.05 = $10.00

Comparison:
  Institutional Cluster: Free (but 2-3 day queue wait)
  Spawn On-Demand: $34.00 (immediate)
  Spawn Spot: $10.00 (immediate, 70% savings)
```

---

### `spawn slurm submit`

Converts and launches a Slurm script in one step.

```bash
spawn slurm submit <script.sbatch> [FLAGS]
```

**Flags:**
- `--yes`, `-y`: Skip confirmation prompt
- `--spot`: Use spot instances
- `--region REGION`: Override region selection
- `--instance-type TYPE`: Override instance type selection
- All standard `spawn launch` flags

**Examples:**
```bash
# Submit with spot instances
spawn slurm submit job.sbatch --spot --yes

# Submit to specific region
spawn slurm submit job.sbatch --region us-west-2

# Override instance type
spawn slurm submit job.sbatch --instance-type c5.4xlarge
```

---

## Slurm Directive Support

### Supported Directives

| Slurm Directive | Spawn Equivalent | Notes |
|----------------|------------------|-------|
| `--job-name=NAME` | `sweep_name: NAME` | Becomes instance name prefix |
| `--array=N-M` | `count: M-N+1` | Creates parameter sweep |
| `--time=HH:MM:SS` | `ttl: Xh` | Auto-terminate after duration |
| `--mem=XG` | Instance type selection | Ensures sufficient RAM |
| `--cpus-per-task=N` | Instance type selection | Ensures sufficient vCPUs |
| `--gres=gpu:N` | GPU instance + count | Selects appropriate GPU instance |
| `--nodes=N` | MPI with N instances | Requires `--mpi` flag |
| `--ntasks-per-node=N` | `mpi_processes_per_node: N` | MPI configuration |
| `--partition=NAME` | AMI selection | Maps to custom AMIs (see below) |

### Unsupported Directives

These directives are **not supported** and will generate warnings:

| Directive | Reason | Workaround |
|-----------|--------|------------|
| `--dependency` | No job orchestration | Use external workflow tools |
| `--exclusive` | Cloud instances are exclusive | N/A (already exclusive) |
| `--constraint` | No node properties | Use `#SPAWN --instance-type` |
| `--account`, `--qos` | No accounting system | Use AWS cost tags |
| `--mail-user` | No built-in notifications | Use AWS SNS/CloudWatch |

---

### Custom #SPAWN Directives

Override spawn's automatic selections with custom directives:

```bash
#!/bin/bash
#SBATCH --job-name=custom-job
#SBATCH --array=1-100
#SBATCH --time=02:00:00

# Custom spawn overrides
#SPAWN --instance-type=c5.2xlarge
#SPAWN --region=us-west-2
#SPAWN --spot=true
#SPAWN --ami=ami-12345678

./my_program $SLURM_ARRAY_TASK_ID
```

**Supported #SPAWN Directives:**
- `--instance-type=TYPE`: Override instance type
- `--region=REGION`: Override region
- `--spot=BOOL`: Enable spot instances
- `--ami=AMI_ID`: Use custom AMI
- `--efs-id=FS_ID`: Mount EFS filesystem
- `--ttl=DURATION`: Override TTL
- `--key-pair=NAME`: Override SSH key

---

## Job Type Examples

### Array Jobs

**Slurm Script:**
```bash
#!/bin/bash
#SBATCH --job-name=param-sweep
#SBATCH --array=1-1000
#SBATCH --time=00:30:00
#SBATCH --mem=2G
#SBATCH --cpus-per-task=1

python train.py --seed $SLURM_ARRAY_TASK_ID
```

**Conversion:**
```bash
spawn slurm convert param-sweep.sbatch --output params.yaml
spawn launch --params params.yaml --spot
```

**Result:**
- 1,000 independent t3.small instances
- Each runs for 30 minutes
- Cost: ~$2.50 with spot instances

---

### GPU Jobs

**Slurm Script:**
```bash
#!/bin/bash
#SBATCH --job-name=gpu-training
#SBATCH --gres=gpu:v100:2
#SBATCH --time=04:00:00
#SBATCH --mem=64G
#SBATCH --cpus-per-task=16

module load cuda/11.8
python train_model.py --gpus 2
```

**Conversion:**
```bash
spawn slurm convert gpu-training.sbatch
```

**Spawn Configuration:**
```yaml
sweep_name: gpu-training
instance_type: p3.8xlarge  # 4× V100 GPUs (using 2)
ttl: 4h
base_command: |
  #!/bin/bash
  # CUDA pre-installed on AWS Deep Learning AMI
  python train_model.py --gpus 2
```

**GPU Instance Types:**
| GPU Count | GPU Type | Instance Type | vCPUs | RAM | Spot Price |
|-----------|----------|---------------|-------|-----|------------|
| 1 | V100 | p3.2xlarge | 8 | 61GB | ~$0.90/hr |
| 2 | V100 | p3.8xlarge | 32 | 244GB | ~$3.60/hr |
| 4 | V100 | p3.16xlarge | 64 | 488GB | ~$7.20/hr |
| 1 | A100 | p4d.24xlarge | 96 | 1,152GB | ~$10/hr |
| 8 | A100 | p4d.24xlarge | 96 | 1,152GB | ~$10/hr |

---

### MPI Jobs

**Slurm Script:**
```bash
#!/bin/bash
#SBATCH --job-name=mpi-simulation
#SBATCH --nodes=8
#SBATCH --ntasks-per-node=4
#SBATCH --time=02:00:00
#SBATCH --mem-per-cpu=2G

module load mpi/openmpi
mpirun ./simulate input.dat
```

**Conversion:**
```bash
spawn slurm convert mpi-simulation.sbatch --output params.yaml
```

**Spawn Configuration:**
```yaml
sweep_name: mpi-simulation
count: 8
instance_type: c5.xlarge  # 4 vCPUs
mpi_enabled: true
mpi_processes_per_node: 4
placement_group: auto  # Ensures low-latency networking
ttl: 2h
base_command: |
  #!/bin/bash
  # OpenMPI pre-installed by spawn
  mpirun -np 32 ./simulate input.dat
```

**Launch:**
```bash
spawn launch --params params.yaml --mpi
```

---

### Partitions and Custom AMIs

Many HPC centers use partitions to group nodes by capability. Spawn supports partition-to-AMI mapping.

**Slurm Script:**
```bash
#!/bin/bash
#SBATCH --job-name=matlab-job
#SBATCH --partition=matlab  # Nodes with MATLAB installed
#SBATCH --time=01:00:00

module load matlab
matlab -batch "my_script"
```

**Mapping Configuration** (`~/.spawn/slurm-mapping.yaml`):
```yaml
partitions:
  matlab:
    ami: ami-matlab-r2023b  # Custom AMI with MATLAB
    instance_type: m5.2xlarge

  python:
    ami: ami-python-3.11  # Custom AMI with Python packages
    instance_type: t3.large

  bioinformatics:
    ami: ami-bio-tools  # Custom AMI with BWA, GATK, etc.
    instance_type: r5.xlarge  # Memory-optimized
```

**Conversion:**
```bash
spawn slurm convert matlab-job.sbatch --config ~/.spawn/slurm-mapping.yaml
```

---

## Migration Workflow

### Phase 1: Assessment (1 week)

**1. Inventory Slurm Scripts:**
```bash
# Find all Slurm scripts
find ~/jobs -name "*.sbatch" -o -name "*.slurm"

# Analyze directive usage
grep -h "#SBATCH" ~/jobs/*.sbatch | sort | uniq -c | sort -rn
```

**2. Check Compatibility:**
```bash
# Test conversion (dry-run)
for script in ~/jobs/*.sbatch; do
  spawn slurm convert "$script" > /dev/null || echo "FAIL: $script"
done
```

**3. Estimate Costs:**
```bash
# Get cost estimates for all jobs
for script in ~/jobs/*.sbatch; do
  echo "=== $script ==="
  spawn slurm estimate "$script"
done > cost_estimates.txt
```

---

### Phase 2: Pilot (2 weeks)

**1. Select Pilot Jobs:**

Choose 3-5 representative jobs:
- 1× small array job (< 100 tasks, < 1 hour each)
- 1× medium array job (100-1000 tasks)
- 1× GPU job (if applicable)
- 1× MPI job (if applicable)

**2. Run Pilots:**
```bash
# Convert and review
spawn slurm convert pilot1.sbatch --output pilot1.yaml
vim pilot1.yaml  # Review and adjust

# Launch (start small!)
spawn launch --params pilot1.yaml --count 10 --spot

# Monitor
spawn list

# Collect results
spawn collect <sweep-id>
```

**3. Compare Results:**
- Verify output correctness
- Compare runtime vs. cluster
- Measure actual costs
- Identify issues

---

### Phase 3: Scale-Up (ongoing)

**1. Create Conversion Templates:**
```bash
# ~/.spawn/job-templates/
├── array-cpu.yaml
├── array-gpu.yaml
├── mpi-job.yaml
└── single-task.yaml
```

**2. Establish Workflow:**
```bash
#!/bin/bash
# submit-to-spawn.sh - Wrapper script

SCRIPT=$1
SPOT=${2:-true}

# Convert
spawn slurm convert "$SCRIPT" --output /tmp/params.yaml

# Review (optional)
if [[ -z "$AUTO_SUBMIT" ]]; then
  $EDITOR /tmp/params.yaml
fi

# Submit
spawn launch --params /tmp/params.yaml ${SPOT:+--spot} --yes
```

**3. Monitor Costs:**
```bash
# Daily cost report
spawn cost --since yesterday

# Monthly budget tracking
spawn cost --since "30 days ago"
```

---

## Cost Comparison

### Example: 1,000-task Array Job

**Specifications:**
- 1,000 independent tasks
- 2 hours per task
- 4 vCPUs, 8GB RAM per task

**Institutional Cluster:**
- **Cost:** $0 (free to user)
- **Queue Wait:** 24-72 hours (typical)
- **Total Time:** 3-5 days

**Spawn On-Demand:**
- **Instance:** t3.xlarge ($0.1664/hour)
- **Cost:** 1,000 tasks × 2h × $0.17 = **$332**
- **Queue Wait:** 0 seconds
- **Total Time:** 2 hours

**Spawn Spot (70% savings):**
- **Instance:** t3.xlarge (~$0.05/hour spot)
- **Cost:** 1,000 tasks × 2h × $0.05 = **$100**
- **Queue Wait:** 0 seconds
- **Total Time:** 2 hours

### Break-Even Analysis

**When is spawn cost-effective?**

```
Value of Time Saved = Hourly Rate × Hours Saved

If (Value of Time Saved) > (Spawn Cost), use spawn
```

**Example:**
- Cluster queue wait: 48 hours
- Researcher hourly rate: $50/hour
- Value of time saved: 48 × $50 = $2,400
- Spawn spot cost: $100
- **Net benefit: $2,300** ✅

**Rule of Thumb:**
- For postdocs/researchers: spawn is cost-effective if it saves > 2 days
- For students: spawn is cost-effective if it saves > 1 week
- For deadline-driven work: spawn is always cost-effective

---

## Advanced Topics

### Module System Mapping

Map `module load` commands to spawn setup:

**Slurm Script:**
```bash
#!/bin/bash
#SBATCH --job-name=modules-test

module load gcc/11.2
module load python/3.11
module load cuda/11.8

./my_program
```

**Mapping Config** (`~/.spawn/slurm-mapping.yaml`):
```yaml
modules:
  "gcc/11.2":
    setup: "export PATH=/usr/bin:$PATH"  # Already installed

  "python/3.11":
    setup: |
      pyenv install 3.11
      pyenv global 3.11
    ami: ami-python311  # Or use custom AMI

  "cuda/11.8":
    setup: ""  # Already in Deep Learning AMI
    ami: ami-dlami-cuda118
```

---

### Environment Variables

Spawn provides Slurm-compatible environment variables:

| Slurm Variable | Spawn Equivalent | Value |
|----------------|------------------|-------|
| `SLURM_ARRAY_TASK_ID` | `SWEEP_INDEX` | Task index (0-based) |
| `SLURM_ARRAY_JOB_ID` | `SWEEP_ID` | Unique sweep ID |
| `SLURM_JOB_NAME` | `SWEEP_NAME` | Job/sweep name |
| `SLURM_CPUS_PER_TASK` | `vCPU_COUNT` | Number of vCPUs |
| `SLURM_MEM_PER_NODE` | `MEMORY_GB` | Instance RAM in GB |
| `SLURM_NODELIST` | `MPI_HOSTS` | List of MPI nodes |

**Migration Strategy:**
```bash
# Option 1: Replace in script
sed 's/SLURM_ARRAY_TASK_ID/SWEEP_INDEX/g' job.sbatch > job-spawn.sbatch

# Option 2: Add aliases in script
export SLURM_ARRAY_TASK_ID=$SWEEP_INDEX
export SLURM_ARRAY_JOB_ID=$SWEEP_ID
```

---

### Handling Dependencies

Slurm's `--dependency` is not directly supported. Use these workarounds:

**Option 1: Sequential Sweeps**
```bash
# Run first sweep
sweep1_id=$(spawn launch --params sweep1.yaml --detach)

# Wait for completion
spawn wait $sweep1_id

# Run second sweep (depends on first)
spawn launch --params sweep2.yaml
```

**Option 2: Workflow Tools**
Use external orchestration:
- **Nextflow**: `nextflow run pipeline.nf -profile spawn`
- **Snakemake**: `snakemake --spawn`
- **Airflow**: Custom spawn operator

**Option 3: AWS Step Functions**
```yaml
# step-functions.yaml
StartAt: Sweep1
States:
  Sweep1:
    Type: Task
    Resource: arn:aws:states:::ecs:runTask.sync
    Parameters:
      Command: ["spawn", "launch", "--params", "sweep1.yaml"]
    Next: Sweep2

  Sweep2:
    Type: Task
    Resource: arn:aws:states:::ecs:runTask.sync
    Parameters:
      Command: ["spawn", "launch", "--params", "sweep2.yaml"]
    End: true
```

---

## Troubleshooting

### Parsing Errors

**Error:** `failed to parse Slurm script: invalid directive`

**Cause:** Unsupported or malformed `#SBATCH` directive

**Solution:**
```bash
# Check for typos
grep "#SBATCH" script.sbatch

# Convert with verbose output
spawn slurm convert script.sbatch --verbose

# Use custom directives for unsupported features
#SPAWN --instance-type=...
```

---

### Instance Type Selection Issues

**Error:** `no instance type matches requirements`

**Cause:** Over-constrained requirements (e.g., 256 vCPUs, 2TB RAM)

**Solution:**
```bash
# Check requirements
grep -E "#SBATCH.*(mem|cpus)" script.sbatch

# Override with custom instance type
spawn slurm submit script.sbatch --instance-type=x1e.32xlarge

# Or use #SPAWN directive
#SPAWN --instance-type=x1e.32xlarge
```

---

### Cost Estimation Questions

**Q:** Why is the estimate higher than expected?

**A:** Check:
1. Instance type selection (may be over-provisioned)
2. Region pricing differences
3. On-demand vs. spot pricing
4. Runtime duration estimates

**Solution:**
```bash
# Use spot instances
spawn slurm submit script.sbatch --spot

# Use cheaper region
spawn slurm submit script.sbatch --region=us-east-1

# Override instance type
spawn slurm submit script.sbatch --instance-type=t3.medium
```

---

### Module Loading Failures

**Error:** `module: command not found`

**Cause:** Environment modules not available in spawn

**Solution:**
```bash
# Option 1: Use conda/pyenv instead
#SPAWN --user-data="conda install -y numpy pandas"

# Option 2: Use custom AMI with modules
#SPAWN --ami=ami-with-modules

# Option 3: Direct package installation
#SPAWN --user-data="apt-get install -y python3-numpy"
```

---

## Examples

See [spawn/examples/slurm/](./examples/slurm/) for complete examples:

- `array-cpu.sbatch` - Basic CPU array job
- `array-gpu.sbatch` - GPU array job
- `mpi-job.sbatch` - Multi-node MPI job
- `dependencies.sh` - Handling job dependencies
- `modules.sbatch` - Module system usage
- `bioinformatics.sbatch` - Bioinformatics pipeline
- `machine-learning.sbatch` - ML hyperparameter sweep

---

## Getting Help

- **Documentation:** https://github.com/spore-host/spore-host
- **Issues:** https://github.com/spore-host/spore-host/issues
- **Examples:** `spawn/examples/slurm/`

**Reporting Bugs:**
```bash
# Include script and error output
spawn slurm convert problematic.sbatch 2>&1 | tee error.log

# Create issue with:
# 1. Slurm script (sanitized)
# 2. Error log
# 3. spawn version (spawn --version)
```
