"""
Modern Airflow DAG using TaskFlow API for spawn parameter sweeps.

The TaskFlow API (Airflow 2.0+) provides a cleaner, more Pythonic
way to define workflows with automatic XCom passing.
"""

from airflow.decorators import dag, task
from airflow.utils.dates import days_ago
import subprocess
import json
import time
from datetime import timedelta


default_args = {
    'owner': 'data-team',
    'retries': 2,
    'retry_delay': timedelta(minutes=5),
}


@dag(
    default_args=default_args,
    schedule_interval='@daily',
    start_date=days_ago(1),
    catchup=False,
    tags=['spawn', 'ec2', 'parameter-sweep'],
    description='Spawn parameter sweep using modern TaskFlow API',
)
def spawn_sweep_taskflow():
    """Modern spawn sweep workflow."""

    @task
    def launch_sweep(params_file: str = "/opt/airflow/config/daily_sweep.yaml") -> str:
        """
        Launch a spawn parameter sweep.

        Returns:
            sweep_id: The sweep ID for status tracking
        """
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

    @task(execution_timeout=timedelta(hours=2))
    def wait_for_completion(sweep_id: str) -> dict:
        """
        Wait for sweep to complete.

        Args:
            sweep_id: Sweep ID from launch_sweep

        Returns:
            status: Final sweep status
        """
        print(f"Waiting for sweep: {sweep_id}")

        start_time = time.time()
        max_wait = 7200  # 2 hours

        while True:
            elapsed = time.time() - start_time

            if elapsed > max_wait:
                raise TimeoutError(f"Sweep timeout after {max_wait}s")

            # Check status
            result = subprocess.run(
                ['spawn', 'status', sweep_id, '--check-complete'],
                capture_output=True
            )

            if result.returncode == 0:
                print(f"✅ Sweep completed: {sweep_id}")
                break
            elif result.returncode == 1:
                raise Exception(f"❌ Sweep failed: {sweep_id}")
            elif result.returncode == 3:
                raise Exception(f"❌ Error checking status: {sweep_id}")

            # Still running
            if int(elapsed) % 300 == 0:
                print(f"⏳ Still running (elapsed: {int(elapsed)}s)")

            time.sleep(60)

        # Get final status
        result = subprocess.run(
            ['spawn', 'status', sweep_id, '--json'],
            capture_output=True,
            text=True,
            check=True
        )

        status = json.loads(result.stdout)
        print(f"Final status: {status['Status']}")
        print(f"Launched: {status['Launched']}/{status['TotalParams']}")

        return status

    @task
    def process_results(status: dict) -> dict:
        """
        Process sweep results.

        Args:
            status: Status from wait_for_completion

        Returns:
            results: Processed results
        """
        print("Processing sweep results...")

        results = {
            'total_params': status['TotalParams'],
            'launched': status['Launched'],
            'failed': status['Failed'],
            'success_rate': status['Launched'] / status['TotalParams'],
        }

        print(f"Success rate: {results['success_rate']:.1%}")

        if results['failed'] > 0:
            print(f"⚠️  {results['failed']} instances failed")

        # Add your custom processing logic here

        return results

    @task
    def send_notification(results: dict):
        """
        Send completion notification.

        Args:
            results: Results from process_results
        """
        print(f"Sending notification: {results}")

        # Example: Send to Slack, email, etc.
        # slack_hook.send(f"Sweep completed with {results['success_rate']:.1%} success rate")

    # Define task dependencies with TaskFlow
    sweep_id = launch_sweep()
    status = wait_for_completion(sweep_id)
    results = process_results(status)
    send_notification(results)


# Alternative: Simplified version using --wait
@dag(
    default_args=default_args,
    schedule_interval='@daily',
    start_date=days_ago(1),
    catchup=False,
    tags=['spawn', 'simple'],
)
def spawn_sweep_simple():
    """Simplified sweep using spawn's --wait flag."""

    @task(execution_timeout=timedelta(hours=2))
    def launch_and_wait(params_file: str = "/opt/airflow/config/daily_sweep.yaml") -> str:
        """Launch sweep and wait for completion in one step."""
        print(f"Launching sweep with --wait: {params_file}")

        result = subprocess.run(
            ['spawn', 'launch',
             '--params', params_file,
             '--detach',
             '--wait',
             '--wait-timeout', '2h',
             '--output-id', '/tmp/spawn_sweep_id.txt'],
            capture_output=True,
            text=True
        )

        if result.returncode != 0:
            raise Exception(f"Sweep failed: {result.stderr}")

        with open('/tmp/spawn_sweep_id.txt') as f:
            sweep_id = f.read().strip()

        print(f"✅ Sweep completed: {sweep_id}")
        return sweep_id

    @task
    def verify_results(sweep_id: str):
        """Verify sweep completed successfully."""
        result = subprocess.run(
            ['spawn', 'status', sweep_id, '--json'],
            capture_output=True,
            text=True,
            check=True
        )

        status = json.loads(result.stdout)

        if status['Status'] != 'COMPLETED':
            raise Exception(f"Unexpected status: {status['Status']}")

        if status['Failed'] > 0:
            print(f"⚠️  {status['Failed']} instances failed")

        print(f"✅ Verified: {status['Launched']}/{status['TotalParams']} launched")

    # Simple linear flow
    sweep_id = launch_and_wait()
    verify_results(sweep_id)


# Instantiate DAGs
spawn_sweep_taskflow_dag = spawn_sweep_taskflow()
spawn_sweep_simple_dag = spawn_sweep_simple()
