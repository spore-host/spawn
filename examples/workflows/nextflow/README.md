# Nextflow Integration Example

## Overview

Nextflow is a workflow system for computational pipelines, widely used in bioinformatics and scientific computing.

## Prerequisites

```bash
# Install Nextflow
curl -s https://get.nextflow.io | bash
chmod +x nextflow
```

## Running

```bash
# Run locally
./nextflow run spawn_sweep.nf

# With custom parameters
./nextflow run spawn_sweep.nf --sweep_file my_sweep.yaml --sweep_timeout 4h

# Resume failed run
./nextflow run spawn_sweep.nf -resume
```

## Features

- Process-based workflow
- Channel-based data flow
- Automatic result publishing
- Resume capability

## See Also

- [Nextflow Documentation](https://www.nextflow.io/docs/latest/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
