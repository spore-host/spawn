# spawn slurm

Run Slurm batch scripts on AWS.

## Synopsis

```bash
spawn slurm <slurm-script> [flags]
```

## Description

Execute Slurm batch scripts on AWS EC2 instances without modification. Translates Slurm directives to spawn equivalents for seamless cloud migration.

## Arguments

#### slurm-script
**Type:** Path
**Required:** Yes
**Description:** Slurm batch script to execute.

```bash
spawn slurm train.slurm
```

## Flags

#### --dry-run
**Type:** Boolean
**Default:** `false`
**Description:** Show translation without launching.

```bash
spawn slurm train.slurm --dry-run
```

## Slurm Directive Translation

| Slurm Directive | spawn Equivalent |
|----------------|------------------|
| `#SBATCH --nodes=N` | `--array N` |
| `#SBATCH --ntasks=N` | `--array N` |
| `#SBATCH --time=HH:MM:SS` | `--ttl` |
| `#SBATCH --partition=gpu` | Auto-select GPU instance |
| `#SBATCH --gres=gpu:N` | Select instance with N GPUs |
| `#SBATCH --cpus-per-task=N` | Select instance with N vCPUs |
| `#SBATCH --mem=NGB` | Select instance with N GB RAM |

## Example Slurm Script

```bash
#!/bin/bash
#SBATCH --job-name=training
#SBATCH --nodes=4
#SBATCH --ntasks-per-node=1
#SBATCH --time=04:00:00
#SBATCH --partition=gpu
#SBATCH --gres=gpu:1

python train.py
```

**Equivalent spawn command:**
```bash
spawn launch \
  --array 4 \
  --instance-type g5.xlarge \
  --ttl 4h \
  --command "python train.py"
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Launch successful |
| 1 | Translation failed (unsupported directive) |
| 2 | Invalid script |

## See Also

- [spawn launch](launch.md) - Direct launch
- [Slurm Migration Guide](../../how-to/slurm-migration.md) - Complete guide
