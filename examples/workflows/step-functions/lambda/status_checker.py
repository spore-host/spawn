"""
AWS Lambda function to check spawn sweep status.

This function is invoked by Step Functions to poll sweep status.
"""

import subprocess
import json


def handler(event, context):
    """
    Check spawn sweep status.

    Input event:
    {
        "sweepId": "sweep-20240101-120000-abc123"
    }

    Output:
    {
        "state": "RUNNING|COMPLETED|FAILED|CANCELLED",
        "sweepId": "sweep-20240101-120000-abc123",
        "details": {...}
    }
    """
    sweep_id = event.get('sweepId')
    if not sweep_id:
        raise ValueError("sweepId is required")

    print(f"Checking status for sweep: {sweep_id}")

    try:
        # Check status with --check-complete for exit codes
        check_result = subprocess.run(
            ['spawn', 'status', sweep_id, '--check-complete'],
            capture_output=True
        )

        # Map exit codes to states
        if check_result.returncode == 0:
            state = "COMPLETED"
        elif check_result.returncode == 1:
            state = "FAILED"
        elif check_result.returncode == 2:
            state = "RUNNING"
        else:
            state = "ERROR"

        # Get detailed status
        status_result = subprocess.run(
            ['spawn', 'status', sweep_id, '--json'],
            capture_output=True,
            text=True,
            check=True
        )

        details = json.loads(status_result.stdout)

        print(f"Status: {state}")
        print(f"Launched: {details.get('Launched', 0)}/{details.get('TotalParams', 0)}")

        return {
            'state': state,
            'sweepId': sweep_id,
            'details': details
        }

    except subprocess.CalledProcessError as e:
        print(f"❌ Status check failed: {e.stderr}")
        raise Exception(f"Failed to check status: {e.stderr}")

    except Exception as e:
        print(f"❌ Unexpected error: {e}")
        raise
