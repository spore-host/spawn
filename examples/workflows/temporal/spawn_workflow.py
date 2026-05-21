"""
Temporal workflow for spawn parameter sweep execution.

Temporal provides durable execution with automatic retries,
timeouts, and long-running workflow support.
"""

import asyncio
import subprocess
import json
from datetime import timedelta
from temporalio import activity, workflow
from temporalio.client import Client
from temporalio.worker import Worker


# Activities - individual units of work that can fail and retry
@activity.defn
async def launch_spawn_sweep(params_file: str) -> str:
    """Launch a spawn parameter sweep."""
    activity.logger.info(f"Launching sweep: {params_file}")

    result = subprocess.run(
        ['spawn', 'launch',
         '--params', params_file,
         '--detach',
         '--output-id', '/tmp/spawn_sweep_id.txt'],
        capture_output=True,
        text=True,
        check=True
    )

    with open('/tmp/spawn_sweep_id.txt') as f:
        sweep_id = f.read().strip()

    activity.logger.info(f"Launched sweep: {sweep_id}")
    return sweep_id


@activity.defn
async def check_sweep_status(sweep_id: str) -> dict:
    """Check sweep status using --check-complete."""
    result = subprocess.run(
        ['spawn', 'status', sweep_id, '--check-complete'],
        capture_output=True
    )

    # Get detailed status
    status_result = subprocess.run(
        ['spawn', 'status', sweep_id, '--json'],
        capture_output=True,
        text=True,
        check=True
    )

    status = json.loads(status_result.stdout)

    return {
        'exit_code': result.returncode,
        'status': status['Status'],
        'launched': status['Launched'],
        'total': status['TotalParams'],
        'failed': status['Failed'],
        'full_status': status
    }


@activity.defn
async def process_sweep_results(status_data: dict) -> dict:
    """Process sweep results."""
    activity.logger.info("Processing sweep results...")

    full_status = status_data['full_status']

    results = {
        'total_params': full_status['TotalParams'],
        'launched': full_status['Launched'],
        'failed': full_status['Failed'],
        'success_rate': full_status['Launched'] / full_status['TotalParams']
    }

    activity.logger.info(f"Success rate: {results['success_rate']:.1%}")

    return results


# Workflow - orchestrates activities with durable execution
@workflow.defn
class SpawnSweepWorkflow:
    """Durable workflow for spawn parameter sweeps."""

    @workflow.run
    async def run(self, params_file: str, max_wait_time: int = 7200) -> dict:
        """
        Run a complete spawn sweep workflow.

        Args:
            params_file: Path to sweep parameters YAML
            max_wait_time: Maximum time to wait for completion (seconds)

        Returns:
            Dictionary with sweep results
        """
        workflow.logger.info(f"Starting spawn sweep workflow: {params_file}")

        # Activity 1: Launch sweep
        # Temporal will automatically retry on transient failures
        sweep_id = await workflow.execute_activity(
            launch_spawn_sweep,
            params_file,
            start_to_close_timeout=timedelta(minutes=5),
            retry_policy=workflow.RetryPolicy(
                initial_interval=timedelta(seconds=10),
                maximum_interval=timedelta(minutes=1),
                maximum_attempts=3,
            ),
        )

        workflow.logger.info(f"Sweep launched: {sweep_id}")

        # Activity 2: Poll for completion
        # Use Temporal timer for durable sleep
        start_time = workflow.now()
        poll_interval = 60  # seconds

        while True:
            # Check if we've exceeded max wait time
            elapsed = (workflow.now() - start_time).total_seconds()
            if elapsed > max_wait_time:
                raise TimeoutError(f"Sweep timeout after {max_wait_time}s")

            # Check status
            status_data = await workflow.execute_activity(
                check_sweep_status,
                sweep_id,
                start_to_close_timeout=timedelta(minutes=2),
                retry_policy=workflow.RetryPolicy(
                    initial_interval=timedelta(seconds=5),
                    maximum_attempts=5,
                ),
            )

            exit_code = status_data['exit_code']
            workflow.logger.info(
                f"Status: {status_data['status']}, "
                f"Progress: {status_data['launched']}/{status_data['total']}"
            )

            if exit_code == 0:
                # Completed
                workflow.logger.info(f"✅ Sweep completed: {sweep_id}")
                break
            elif exit_code == 1:
                # Failed
                raise Exception(f"❌ Sweep failed: {sweep_id}")
            elif exit_code == 3:
                # Error
                raise Exception(f"❌ Error checking status: {sweep_id}")

            # Still running (exit_code == 2)
            # Use Temporal timer for durable sleep
            await asyncio.sleep(poll_interval)

        # Activity 3: Process results
        results = await workflow.execute_activity(
            process_sweep_results,
            status_data,
            start_to_close_timeout=timedelta(minutes=10),
        )

        workflow.logger.info(f"Workflow completed: {results}")
        return results


# Alternative: Simplified workflow using --wait
@workflow.defn
class SimpleSpawnSweepWorkflow:
    """Simplified workflow using spawn's --wait flag."""

    @workflow.run
    async def run(self, params_file: str, timeout: str = "2h") -> str:
        """Run sweep with --wait flag."""

        @activity.defn
        async def launch_and_wait(params_file: str, timeout: str) -> str:
            """Launch sweep and wait for completion."""
            result = subprocess.run(
                ['spawn', 'launch',
                 '--params', params_file,
                 '--detach',
                 '--wait',
                 '--wait-timeout', timeout,
                 '--output-id', '/tmp/sweep_id.txt'],
                capture_output=True,
                text=True
            )

            if result.returncode != 0:
                raise Exception(f"Sweep failed: {result.stderr}")

            with open('/tmp/sweep_id.txt') as f:
                return f.read().strip()

        sweep_id = await workflow.execute_activity(
            launch_and_wait,
            args=[params_file, timeout],
            start_to_close_timeout=timedelta(hours=3),
        )

        return sweep_id


# Main execution
async def main():
    """Run the workflow."""
    # Connect to Temporal server
    client = await Client.connect("localhost:7233")

    # Run a worker
    async with Worker(
        client,
        task_queue="spawn-sweep-queue",
        workflows=[SpawnSweepWorkflow, SimpleSpawnSweepWorkflow],
        activities=[launch_spawn_sweep, check_sweep_status, process_sweep_results],
    ):
        # Start workflow
        result = await client.execute_workflow(
            SpawnSweepWorkflow.run,
            "sweep.yaml",
            id="spawn-sweep-001",
            task_queue="spawn-sweep-queue",
        )

        print(f"Workflow completed: {result}")


if __name__ == "__main__":
    asyncio.run(main())
