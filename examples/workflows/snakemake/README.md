# Snakemake Integration Example

## Overview

Snakemake is a workflow management system for reproducible and scalable data analysis.

## Prerequisites

```bash
pip install snakemake
```

## Running

```bash
# Run workflow
snakemake --cores 1

# With custom config
snakemake --cores 1 --config sweep_file=my_sweep.yaml

# Dry run
snakemake -n

# With conda
snakemake --use-conda --cores 1
```

## See Also

- [Snakemake Documentation](https://snakemake.readthedocs.io/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
