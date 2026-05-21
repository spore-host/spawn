# Dagster Integration Example

## Overview

Dagster is a modern data orchestrator with a focus on data assets, their dependencies, and observability.

## Prerequisites

```bash
pip install dagster dagster-webserver
```

## Running

### Asset-Based Workflow (Recommended)

```bash
# Start Dagster UI
dagster dev -f spawn_assets.py

# Access UI at http://localhost:3000
# Materialize the "sweep_results" asset (will auto-materialize dependencies)
```

### Job-Based Workflow

```bash
# Run job directly
dagster job execute -f spawn_assets.py -j spawn_sweep_job
```

## Features Demonstrated

### Assets
- **sweep_id**: Launches sweep and returns ID
- **sweep_completion**: Waits for completion (depends on sweep_id)
- **sweep_results**: Processes results (depends on sweep_completion)

Dagster automatically resolves dependencies and ensures correct execution order.

### Jobs
Alternative approach using ops for more imperative workflows.

## Configuration

```python
# In Dagster UI or via config YAML
{
  "ops": {
    "launch_sweep_op": {
      "config": {
        "params_file": "/path/to/sweep.yaml"
      }
    }
  }
}
```

## Benefits of Dagster

- **Asset lineage**: Track how data flows through your pipeline
- **Incremental materialization**: Only recompute what changed
- **Rich UI**: Visual pipeline editor and monitoring
- **Type safety**: Dagster validates asset types at definition time
- **Retry policies**: Automatic retry on transient failures

## See Also

- [Dagster Documentation](https://docs.dagster.io/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
