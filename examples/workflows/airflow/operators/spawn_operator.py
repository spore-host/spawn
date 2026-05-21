"""
Custom Airflow operator for spawn parameter sweeps.

This operator provides a cleaner interface for launching and waiting for
spawn sweeps directly from Airflow DAGs.
"""

from airflow.models import BaseOperator
from airflow.utils.decorators import apply_defaults
import subprocess
import time
import json


class SpawnSweepOperator(BaseOperator):
    """
    Launch and monitor a spawn parameter sweep.

    This operator launches a detached spawn sweep and polls until completion.

    :param params_file: Path to sweep parameters file (YAML/JSON)
    :param timeout: Maximum time to wait for completion (seconds)
    :param poll_interval: Time between status checks (seconds)
    :param wait_timeout: Timeout string for spawn --wait-timeout (e.g., "2h")
    :param use_wait: Use spawn's built-in --wait instead of manual polling
    """

    template_fields = ('params_file', 'wait_timeout')
    ui_color = '#ff6b6b'

    @apply_defaults
    def __init__(
        self,
        params_file,
        timeout=7200,
        poll_interval=60,
        wait_timeout='2h',
        use_wait=False,
        *args,
        **kwargs
    ):
        super().__init__(*args, **kwargs)
        self.params_file = params_file
        self.timeout = timeout
        self.poll_interval = poll_interval
        self.wait_timeout = wait_timeout
        self.use_wait = use_wait

    def execute(self, context):
        """Execute the spawn sweep."""

        # Use spawn's built-in --wait for simplicity
        if self.use_wait:
            return self._execute_with_wait(context)

        # Manual polling for more control
        return self._execute_with_polling(context)

    def _execute_with_wait(self, context):
        """Launch sweep using spawn's built-in --wait flag."""
        self.log.info(f"Launching sweep with --wait: {self.params_file}")

        cmd = [
            'spawn', 'launch',
            '--params', self.params_file,
            '--detach',
            '--wait',
            '--wait-timeout', self.wait_timeout,
            '--output-id', '/tmp/spawn_sweep_id.txt'
        ]

        try:
            result = subprocess.run(
                cmd,
                capture_output=True,
                text=True,
                check=True
            )

            # Read sweep ID
            with open('/tmp/spawn_sweep_id.txt') as f:
                sweep_id = f.read().strip()

            self.log.info(f"Sweep completed: {sweep_id}")
            return sweep_id

        except subprocess.CalledProcessError as e:
            self.log.error(f"Sweep failed: {e.stderr}")
            raise

    def _execute_with_polling(self, context):
        """Launch sweep and poll manually for completion."""
        self.log.info(f"Launching detached sweep: {self.params_file}")

        # Launch sweep
        cmd = [
            'spawn', 'launch',
            '--params', self.params_file,
            '--detach',
            '--output-id', '/tmp/spawn_sweep_id.txt'
        ]

        try:
            result = subprocess.run(
                cmd,
                capture_output=True,
                text=True,
                check=True
            )
        except subprocess.CalledProcessError as e:
            self.log.error(f"Failed to launch sweep: {e.stderr}")
            raise

        # Read sweep ID
        with open('/tmp/spawn_sweep_id.txt') as f:
            sweep_id = f.read().strip()

        self.log.info(f"Launched sweep: {sweep_id}")
        self.log.info(f"Polling for completion (timeout: {self.timeout}s)...")

        # Poll for completion
        start_time = time.time()
        while True:
            elapsed = time.time() - start_time

            if elapsed > self.timeout:
                raise Exception(
                    f"Sweep timeout after {self.timeout}s. "
                    f"Sweep ID: {sweep_id}"
                )

            # Check status
            result = subprocess.run(
                ['spawn', 'status', sweep_id, '--check-complete'],
                capture_output=True
            )

            if result.returncode == 0:
                self.log.info(f"✅ Sweep completed successfully: {sweep_id}")
                self._log_sweep_stats(sweep_id)
                return sweep_id

            elif result.returncode == 1:
                self.log.error(f"❌ Sweep failed: {sweep_id}")
                self._log_sweep_stats(sweep_id)
                raise Exception(f"Sweep failed: {sweep_id}")

            elif result.returncode == 3:
                self.log.error(f"❌ Error querying sweep status: {sweep_id}")
                raise Exception(f"Error querying status for sweep: {sweep_id}")

            # Still running (exit code 2)
            self.log.info(
                f"⏳ Sweep still running... "
                f"(elapsed: {int(elapsed)}s, sweep: {sweep_id})"
            )
            time.sleep(self.poll_interval)

    def _log_sweep_stats(self, sweep_id):
        """Log sweep statistics."""
        try:
            result = subprocess.run(
                ['spawn', 'status', sweep_id, '--json'],
                capture_output=True,
                text=True,
                check=True
            )

            status = json.loads(result.stdout)

            self.log.info("Sweep Statistics:")
            self.log.info(f"  Status: {status.get('Status')}")
            self.log.info(f"  Total Parameters: {status.get('TotalParams')}")
            self.log.info(f"  Launched: {status.get('Launched')}")
            self.log.info(f"  Failed: {status.get('Failed')}")

        except Exception as e:
            self.log.warning(f"Could not fetch sweep stats: {e}")
