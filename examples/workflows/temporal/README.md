# Temporal Integration Example

## Overview

Temporal provides durable execution for long-running workflows with automatic retries, fault tolerance, and visibility.

## Prerequisites

```bash
# Install Temporal Python SDK
pip install temporalio

# Start Temporal server (Docker)
git clone https://github.com/temporalio/docker-compose.git temporal-docker
cd temporal-docker
docker-compose up -d
```

## Running

### Start Worker

```bash
# Terminal 1: Start worker to process workflows
python spawn_workflow.py
```

### Execute Workflow

```bash
# Terminal 2: Start a workflow
temporal workflow start \
    --type SpawnSweepWorkflow \
    --task-queue spawn-sweep-queue \
    --workflow-id spawn-sweep-001 \
    --input '"sweep.yaml"'

# Watch workflow progress
temporal workflow show --workflow-id spawn-sweep-001
```

### Via Python Client

```python
from temporalio.client import Client
import asyncio

async def run_workflow():
    client = await Client.connect("localhost:7233")

    result = await client.execute_workflow(
        "SpawnSweepWorkflow",
        "sweep.yaml",
        id="spawn-sweep-001",
        task_queue="spawn-sweep-queue",
    )

    print(f"Results: {result}")

asyncio.run(run_workflow())
```

## Features Demonstrated

### Durable Execution
```python
# Workflow survives process restarts
await asyncio.sleep(60)  # Temporal timer, not regular sleep
```

If the worker crashes, Temporal resumes from the last checkpoint.

### Automatic Retries
```python
await workflow.execute_activity(
    launch_spawn_sweep,
    retry_policy=workflow.RetryPolicy(
        initial_interval=timedelta(seconds=10),
        maximum_attempts=3,
    ),
)
```

Transient failures are automatically retried with exponential backoff.

### Long-Running Workflows
```python
# Can run for hours/days without issues
while True:
    status = await workflow.execute_activity(check_sweep_status, sweep_id)
    if status['exit_code'] == 0:
        break
    await asyncio.sleep(60)  # Durable sleep
```

### Observability
```
# View workflow history
temporal workflow show --workflow-id spawn-sweep-001

# See detailed event history
temporal workflow show --workflow-id spawn-sweep-001 --show-history
```

## Why Use Temporal with spawn?

1. **Fault Tolerance**: Worker crashes don't lose progress
2. **Visibility**: See complete execution history in Temporal UI
3. **Debugging**: Replay workflows to debug issues
4. **Scalability**: Multiple workers can process workflows in parallel
5. **Guaranteed Execution**: Workflows always complete or explicitly fail

## Workflow Patterns

### Basic Pattern
```python
sweep_id = await workflow.execute_activity(launch_spawn_sweep, params_file)
# Wait for completion with polling
results = await workflow.execute_activity(process_results, status_data)
```

### Simplified Pattern (using --wait)
```python
# One activity handles everything
sweep_id = await workflow.execute_activity(launch_and_wait, params_file, timeout)
```

## See Also

- [Temporal Documentation](https://docs.temporal.io/)
- [Temporal Python SDK](https://github.com/temporalio/sdk-python)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
