"""
Luigi pipeline for spawn parameter sweep execution.

Luigi is Spotify's workflow orchestration tool with a focus on
batch processing and dependency resolution.
"""

import luigi
import subprocess
import json
import time
from pathlib import Path


class LaunchSweep(luigi.Task):
    """Launch a spawn parameter sweep."""

    params_file = luigi.Parameter()
    output_dir = luigi.Parameter(default="/tmp/spawn")

    def output(self):
        """Sweep ID file is the output."""
        return luigi.LocalTarget(f"{self.output_dir}/sweep_id.txt")

    def run(self):
        """Launch the sweep."""
        print(f"Launching sweep: {self.params_file}")

        Path(self.output_dir).mkdir(parents=True, exist_ok=True)

        result = subprocess.run(
            ['spawn', 'launch',
             '--params', self.params_file,
             '--detach',
             '--output-id', self.output().path],
            capture_output=True,
            text=True,
            check=True
        )

        print(f"Sweep launched: {self.output().path}")


class WaitForSweep(luigi.Task):
    """Wait for sweep to complete."""

    params_file = luigi.Parameter()
    output_dir = luigi.Parameter(default="/tmp/spawn")
    timeout_seconds = luigi.IntParameter(default=7200)
    poll_interval = luigi.IntParameter(default=60)

    def requires(self):
        """Depends on LaunchSweep."""
        return LaunchSweep(
            params_file=self.params_file,
            output_dir=self.output_dir
        )

    def output(self):
        """Status JSON file is the output."""
        return luigi.LocalTarget(f"{self.output_dir}/sweep_status.json")

    def run(self):
        """Wait for sweep completion."""
        # Read sweep ID from previous task
        with self.input().open('r') as f:
            sweep_id = f.read().strip()

        print(f"Waiting for sweep: {sweep_id}")

        start_time = time.time()

        while True:
            elapsed = time.time() - start_time

            if elapsed > self.timeout_seconds:
                raise Exception(f"Sweep timeout after {self.timeout_seconds}s")

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
                raise Exception(f"❌ Error querying status: {sweep_id}")

            # Still running
            if int(elapsed) % 300 == 0:
                print(f"⏳ Still running (elapsed: {int(elapsed)}s)")

            time.sleep(self.poll_interval)

        # Get final status
        result = subprocess.run(
            ['spawn', 'status', sweep_id, '--json'],
            capture_output=True,
            text=True,
            check=True
        )

        # Write status to output file
        with self.output().open('w') as f:
            f.write(result.stdout)


class ProcessResults(luigi.Task):
    """Process sweep results."""

    params_file = luigi.Parameter()
    output_dir = luigi.Parameter(default="/tmp/spawn")

    def requires(self):
        """Depends on WaitForSweep."""
        return WaitForSweep(
            params_file=self.params_file,
            output_dir=self.output_dir
        )

    def output(self):
        """Results file is the output."""
        return luigi.LocalTarget(f"{self.output_dir}/results.txt")

    def run(self):
        """Process the results."""
        # Read status from previous task
        with self.input().open('r') as f:
            status = json.load(f)

        print("Processing sweep results...")
        print(f"  Status: {status['Status']}")
        print(f"  Total Params: {status['TotalParams']}")
        print(f"  Launched: {status['Launched']}")
        print(f"  Failed: {status['Failed']}")

        success_rate = status['Launched'] / status['TotalParams']
        print(f"  Success Rate: {success_rate:.1%}")

        # Write results
        with self.output().open('w') as f:
            f.write(f"Sweep Results\n")
            f.write(f"=============\n")
            f.write(f"Status: {status['Status']}\n")
            f.write(f"Success Rate: {success_rate:.1%}\n")
            f.write(f"Total Params: {status['TotalParams']}\n")
            f.write(f"Launched: {status['Launched']}\n")
            f.write(f"Failed: {status['Failed']}\n")

        print(f"✅ Results written to: {self.output().path}")


# Alternative: Simplified version using --wait
class SimpleSweepTask(luigi.Task):
    """Simplified sweep task using --wait flag."""

    params_file = luigi.Parameter()
    output_dir = luigi.Parameter(default="/tmp/spawn")

    def output(self):
        return luigi.LocalTarget(f"{self.output_dir}/sweep_complete.txt")

    def run(self):
        """Launch and wait in one step."""
        print(f"Launching sweep with --wait: {self.params_file}")

        Path(self.output_dir).mkdir(parents=True, exist_ok=True)

        # Use --wait for simplicity
        result = subprocess.run(
            ['spawn', 'launch',
             '--params', self.params_file,
             '--detach',
             '--wait',
             '--wait-timeout', '2h',
             '--output-id', f"{self.output_dir}/sweep_id.txt"],
            capture_output=True,
            text=True
        )

        if result.returncode != 0:
            raise Exception(f"Sweep failed: {result.stderr}")

        # Write completion marker
        with self.output().open('w') as f:
            f.write("Sweep completed successfully\n")


if __name__ == '__main__':
    luigi.run()
