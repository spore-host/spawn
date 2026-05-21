"""
Prefect flow for spawn parameter sweep execution.

This flow demonstrates modern workflow patterns with Prefect:
- Task-based decomposition
- Automatic retry logic
- Progress tracking
- Error handling
"""

from prefect import flow, task
from prefect.tasks import task_input_hash
from datetime import timedelta
import subprocess
import time
import json


@task(retries=3, retry_delay_seconds=60)
def launch_spawn_sweep(params_file: str) -> str:
    """Launch a spawn parameter sweep."""
    print(f"Launching sweep: {params_file}")

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

    print(f"✅ Launched sweep: {sweep_id}")
    return sweep_id


@task(timeout_seconds=7200)
def wait_for_sweep(sweep_id: str) -> dict:
    """Wait for sweep to complete and return status."""
    print(f"Waiting for sweep: {sweep_id}")

    start_time = time.time()
    poll_count = 0

    while True:
        result = subprocess.run(
            ['spawn', 'status', sweep_id, '--check-complete'],
            capture_output=True
        )

        poll_count += 1
        elapsed = int(time.time() - start_time)

        if result.returncode == 0:
            print(f"✅ Sweep completed! (elapsed: {elapsed}s, polls: {poll_count})")
            return get_sweep_stats(sweep_id)

        elif result.returncode == 1:
            stats = get_sweep_stats(sweep_id)
            raise Exception(f"Sweep failed: {stats.get('ErrorMessage', 'Unknown error')}")

        elif result.returncode == 3:
            raise Exception("Error querying sweep status!")

        # Still running
        if poll_count % 5 == 0:  # Log every 5 polls (5 minutes)
            print(f"⏳ Sweep running... (elapsed: {elapsed}s)")

        time.sleep(60)


@task(cache_key_fn=task_input_hash, cache_expiration=timedelta(hours=1))
def get_sweep_stats(sweep_id: str) -> dict:
    """Get detailed sweep statistics."""
    result = subprocess.run(
        ['spawn', 'status', sweep_id, '--json'],
        capture_output=True,
        text=True,
        check=True
    )

    return json.loads(result.stdout)


@task
def process_results(sweep_stats: dict):
    """Process sweep results."""
    print("Processing sweep results...")
    print(f"  Status: {sweep_stats.get('Status')}")
    print(f"  Total Params: {sweep_stats.get('TotalParams')}")
    print(f"  Launched: {sweep_stats.get('Launched')}")
    print(f"  Failed: {sweep_stats.get('Failed')}")

    if sweep_stats.get('Failed', 0) > 0:
        print("⚠️  Some instances failed - review logs")

    # Add your result processing logic here
    return True


@flow(name="spawn-parameter-sweep")
def spawn_sweep_flow(params_file: str):
    """Complete workflow for spawn parameter sweep."""
    sweep_id = launch_spawn_sweep(params_file)
    sweep_stats = wait_for_sweep(sweep_id)
    process_results(sweep_stats)


@flow(name="parallel-sweeps")
def parallel_sweeps_flow(params_files: list[str]):
    """Launch multiple sweeps in parallel."""
    # Launch all sweeps concurrently
    sweep_ids = []
    for params_file in params_files:
        sweep_id = launch_spawn_sweep.submit(params_file)
        sweep_ids.append(sweep_id)

    # Wait for all to complete
    results = []
    for sweep_id_future in sweep_ids:
        sweep_id = sweep_id_future.result()
        stats = wait_for_sweep.submit(sweep_id)
        results.append(stats)

    # Process all results
    for stats_future in results:
        stats = stats_future.result()
        process_results(stats)


@flow(name="conditional-sweep")
def conditional_sweep_flow(params_file: str, followup_file: str = None):
    """Run sweep with optional follow-up based on results."""
    # Run initial sweep
    sweep_id = launch_spawn_sweep(params_file)
    sweep_stats = wait_for_sweep(sweep_id)

    # Check if follow-up needed
    if followup_file and sweep_stats.get('Failed', 0) == 0:
        print("Initial sweep succeeded, running follow-up...")
        followup_id = launch_spawn_sweep(followup_file)
        followup_stats = wait_for_sweep(followup_id)
        process_results(followup_stats)
    else:
        print("Skipping follow-up (failures detected or no follow-up specified)")

    process_results(sweep_stats)


if __name__ == "__main__":
    # Simple single sweep
    spawn_sweep_flow("/path/to/sweep.yaml")

    # Parallel sweeps
    # parallel_sweeps_flow([
    #     "/path/to/sweep1.yaml",
    #     "/path/to/sweep2.yaml",
    #     "/path/to/sweep3.yaml"
    # ])

    # Conditional sweep
    # conditional_sweep_flow(
    #     "/path/to/initial.yaml",
    #     followup_file="/path/to/followup.yaml"
    # )
