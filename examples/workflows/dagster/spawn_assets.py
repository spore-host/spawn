"""
Dagster assets for spawn parameter sweep execution.

Dagster is a modern data orchestrator with a focus on data assets and
their dependencies.
"""

from dagster import asset, AssetExecutionContext, Definitions, Config
import subprocess
import json
import time
from pathlib import Path


class SpawnSweepConfig(Config):
    """Configuration for spawn sweep."""
    params_file: str
    timeout_seconds: int = 7200
    poll_interval: int = 60


@asset
def sweep_id(context: AssetExecutionContext, config: SpawnSweepConfig) -> str:
    """Launch a spawn parameter sweep and return the sweep ID."""
    context.log.info(f"Launching sweep: {config.params_file}")

    output_file = Path("/tmp/spawn_sweep_id.txt")

    result = subprocess.run(
        ['spawn', 'launch',
         '--params', config.params_file,
         '--detach',
         '--output-id', str(output_file)],
        capture_output=True,
        text=True,
        check=True
    )

    sweep_id = output_file.read_text().strip()
    context.log.info(f"Launched sweep: {sweep_id}")

    return sweep_id


@asset(deps=[sweep_id])
def sweep_completion(context: AssetExecutionContext, sweep_id: str, config: SpawnSweepConfig) -> dict:
    """Wait for sweep to complete and return final status."""
    context.log.info(f"Waiting for sweep: {sweep_id}")

    start_time = time.time()

    while True:
        elapsed = time.time() - start_time

        if elapsed > config.timeout_seconds:
            raise Exception(f"Sweep timeout after {config.timeout_seconds}s")

        # Check status
        result = subprocess.run(
            ['spawn', 'status', sweep_id, '--check-complete'],
            capture_output=True
        )

        if result.returncode == 0:
            context.log.info(f"Sweep completed: {sweep_id}")
            break
        elif result.returncode == 1:
            raise Exception(f"Sweep failed: {sweep_id}")
        elif result.returncode == 3:
            raise Exception(f"Error querying status: {sweep_id}")

        # Still running
        if int(elapsed) % 300 == 0:  # Log every 5 minutes
            context.log.info(f"Sweep still running (elapsed: {int(elapsed)}s)")

        time.sleep(config.poll_interval)

    # Get final status
    result = subprocess.run(
        ['spawn', 'status', sweep_id, '--json'],
        capture_output=True,
        text=True,
        check=True
    )

    status = json.loads(result.stdout)
    context.log.info(f"Final status: {status['Status']}")
    context.log.info(f"Launched: {status['Launched']}/{status['TotalParams']}")
    context.log.info(f"Failed: {status['Failed']}")

    return status


@asset(deps=[sweep_completion])
def sweep_results(context: AssetExecutionContext, sweep_completion: dict) -> dict:
    """Process sweep results."""
    context.log.info("Processing sweep results...")

    # Extract key metrics
    results = {
        'total_params': sweep_completion['TotalParams'],
        'launched': sweep_completion['Launched'],
        'failed': sweep_completion['Failed'],
        'success_rate': sweep_completion['Launched'] / sweep_completion['TotalParams'],
    }

    context.log.info(f"Success rate: {results['success_rate']:.1%}")

    # Add your custom processing logic here

    return results


# Alternative: Use jobs for more complex workflows
from dagster import job, op


@op
def launch_sweep_op(context, params_file: str) -> str:
    """Launch sweep operation."""
    result = subprocess.run(
        ['spawn', 'launch', '--params', params_file,
         '--detach', '--output-id', '/tmp/sweep_id.txt'],
        capture_output=True,
        text=True,
        check=True
    )

    with open('/tmp/sweep_id.txt') as f:
        sweep_id = f.read().strip()

    context.log.info(f"Launched: {sweep_id}")
    return sweep_id


@op
def wait_sweep_op(context, sweep_id: str) -> dict:
    """Wait for sweep completion operation."""
    context.log.info(f"Waiting for: {sweep_id}")

    # Simple approach using --wait
    result = subprocess.run(
        ['spawn', 'launch', '--params', 'sweep.yaml',
         '--detach', '--wait', '--wait-timeout', '2h'],
        capture_output=True,
        text=True
    )

    if result.returncode != 0:
        raise Exception(f"Sweep failed: {result.stderr}")

    # Get status
    result = subprocess.run(
        ['spawn', 'status', sweep_id, '--json'],
        capture_output=True,
        text=True,
        check=True
    )

    return json.loads(result.stdout)


@job
def spawn_sweep_job():
    """Job-based sweep workflow."""
    sweep_id = launch_sweep_op()
    status = wait_sweep_op(sweep_id)


# Define Dagster definitions
defs = Definitions(
    assets=[sweep_id, sweep_completion, sweep_results],
    jobs=[spawn_sweep_job],
)
