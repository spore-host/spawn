## `spawn slurm`

Parse and convert Slurm batch scripts to spawn parameter files.

This enables HPC users to migrate existing Slurm workflows to the cloud
with minimal changes. Supports array jobs, MPI jobs, and GPU jobs.

Examples:
  # Convert Slurm script to spawn parameters
  spawn slurm convert job.sbatch --output params.yaml

  # Estimate cost before running
  spawn slurm estimate job.sbatch

  # Convert and submit in one step
  spawn slurm submit job.sbatch --spot

```
spawn slurm
```

### `spawn slurm convert`

Parse a Slurm batch script and convert it to spawn parameter format.

The generated parameter file can be reviewed and edited before launching.

Supported Slurm directives:
  --array=N-M          → Parameter sweep with M-N+1 tasks
  --time=HH:MM:SS      → TTL for each instance
  --mem=XGB            → Memory requirement for instance selection
  --cpus-per-task=N    → CPU requirement for instance selection
  --gres=gpu:N         → GPU requirement and instance selection
  --nodes=N            → Multi-node MPI job (requires --mpi flag)
  --job-name=NAME      → Instance name prefix

Custom #SPAWN directives (optional):
  #SPAWN --instance-type=TYPE  → Override instance type selection
  #SPAWN --region=REGION       → Override region
  #SPAWN --spot=true           → Enable spot instances
  #SPAWN --ami=AMI_ID          → Override AMI

Example:
  spawn slurm convert train.sbatch --output params.yaml
  spawn launch --params params.yaml

```
spawn slurm convert <script.sbatch> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output-file` |  | string |  | Output parameter file (default: stdout) |

### `spawn slurm estimate`

Parse a Slurm batch script and estimate the cloud cost.

Provides a cost comparison between institutional cluster (free but queued)
and cloud (paid but immediate).

Example:
  spawn slurm estimate train.sbatch

```
spawn slurm estimate <script.sbatch>
```

### `spawn slurm submit`

Parse a Slurm batch script, convert to spawn parameters, and launch immediately.

This is a convenience command that combines 'convert' and 'launch' in one step.
For complex jobs, consider using 'convert' first to review the generated parameters.

Example:
  spawn slurm submit train.sbatch --spot --yes

```
spawn slurm submit <script.sbatch> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip confirmation prompt |

