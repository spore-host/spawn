# Luigi Integration Example

## Overview

Luigi is Spotify's workflow orchestration tool focused on batch processing with automatic dependency resolution.

## Prerequisites

```bash
pip install luigi
```

## Running

### Full Pipeline (Recommended)

```bash
# Run the complete pipeline
python spawn_pipeline.py ProcessResults --params-file sweep.yaml --output-dir /tmp/spawn

# Luigi will automatically run: LaunchSweep → WaitForSweep → ProcessResults
```

### Individual Tasks

```bash
# Just launch (doesn't wait)
python spawn_pipeline.py LaunchSweep --params-file sweep.yaml

# Launch and wait
python spawn_pipeline.py WaitForSweep --params-file sweep.yaml

# Process results (runs all dependencies)
python spawn_pipeline.py ProcessResults --params-file sweep.yaml
```

### Simplified Version

```bash
# Single task using --wait
python spawn_pipeline.py SimpleSweepTask --params-file sweep.yaml
```

## With Luigi Central Scheduler

```bash
# Start Luigi scheduler
luigid

# Run with scheduler (enables parallel execution and visualization)
python spawn_pipeline.py ProcessResults --params-file sweep.yaml --output-dir /tmp/spawn

# Access UI at http://localhost:8082
```

## Features Demonstrated

### Task Dependencies
```python
class WaitForSweep(luigi.Task):
    def requires(self):
        return LaunchSweep(...)  # Automatic dependency
```

Luigi ensures `LaunchSweep` completes before `WaitForSweep` starts.

### Output Targets
```python
def output(self):
    return luigi.LocalTarget("sweep_id.txt")
```

Luigi uses output files to track task completion and avoid re-running.

### Parameter Passing
```python
params_file = luigi.Parameter()
timeout = luigi.IntParameter(default=7200)
```

Command-line arguments become task parameters automatically.

## Benefits of Luigi

- **Automatic dependency resolution**: Declare dependencies, Luigi handles execution order
- **Idempotency**: Tasks that already completed won't re-run
- **Failure recovery**: Resume from last successful task
- **Visual monitoring**: Built-in UI for pipeline visualization
- **Simple Python**: Just inherit from `luigi.Task`

## See Also

- [Luigi Documentation](https://luigi.readthedocs.io/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
