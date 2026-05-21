# Prefect Integration Example

## Overview

Prefect provides a modern Python API for workflow orchestration with powerful features like automatic retries, dynamic task generation, and rich observability.

## Prerequisites

```bash
pip install prefect
```

## Quick Start

```bash
# Run flow locally
python spawn_flow.py

# Deploy to Prefect Cloud
prefect deploy
```

## Features Demonstrated

- **Task retries**: Automatic retry on transient failures
- **Timeout handling**: Task-level timeout configuration
- **Parallel execution**: Launch multiple sweeps concurrently
- **Conditional logic**: Dynamic workflow based on results
- **Result caching**: Cache sweep stats for efficiency

## Usage

```python
from spawn_flow import spawn_sweep_flow

# Run single sweep
spawn_sweep_flow("/path/to/sweep.yaml")
```

## See Also

- [Prefect Documentation](https://docs.prefect.io/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
