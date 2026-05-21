"""
AWS Lambda function to launch spawn parameter sweeps.

This function is invoked by Step Functions to launch a detached sweep.
"""

import subprocess
import json
import os


def handler(event, context):
    """
    Launch a spawn parameter sweep.

    Input event:
    {
        "params_file": "/path/to/sweep.yaml"
    }

    Output:
    {
        "sweepId": "sweep-20240101-120000-abc123"
    }
    """
    params_file = event.get('params_file')
    if not params_file:
        raise ValueError("params_file is required")

    print(f"Launching spawn sweep: {params_file}")

    # Launch detached sweep
    try:
        result = subprocess.run(
            ['spawn', 'launch',
             '--params', params_file,
             '--detach',
             '--output-id', '/tmp/sweep_id.txt'],
            capture_output=True,
            text=True,
            check=True
        )

        print(f"Launch output: {result.stderr}")

        # Read sweep ID
        with open('/tmp/sweep_id.txt') as f:
            sweep_id = f.read().strip()

        print(f"✅ Launched sweep: {sweep_id}")

        return {
            'sweepId': sweep_id,
            'status': 'LAUNCHED'
        }

    except subprocess.CalledProcessError as e:
        print(f"❌ Launch failed: {e.stderr}")
        raise Exception(f"Failed to launch sweep: {e.stderr}")

    except Exception as e:
        print(f"❌ Unexpected error: {e}")
        raise
