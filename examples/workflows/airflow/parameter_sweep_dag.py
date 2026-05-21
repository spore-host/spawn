"""
Apache Airflow DAG for spawn parameter sweep execution.

This DAG demonstrates how to:
1. Launch a spawn parameter sweep
2. Poll for completion
3. Process results
4. Handle failures
"""

from airflow import DAG
from airflow.operators.bash import BashOperator
from airflow.sensors.bash import BashSensor
from airflow.operators.python import PythonOperator
from datetime import datetime, timedelta
import subprocess

default_args = {
    'owner': 'data-team',
    'depends_on_past': False,
    'start_date': datetime(2024, 1, 1),
    'email': ['team@example.com'],
    'email_on_failure': True,
    'email_on_retry': False,
    'retries': 1,
    'retry_delay': timedelta(minutes=5),
}

with DAG(
    'spawn_parameter_sweep',
    default_args=default_args,
    description='Launch and monitor spawn parameter sweep',
    schedule_interval='@daily',
    catchup=False,
    tags=['spawn', 'ec2', 'compute'],
) as dag:

    # Task 1: Launch detached sweep
    launch_sweep = BashOperator(
        task_id='launch_sweep',
        bash_command=(
            'spawn launch '
            '--params /opt/airflow/config/daily_sweep.yaml '
            '--detach '
            '--output-id /tmp/sweep_id.txt'
        ),
        do_xcom_push=True,
    )

    # Task 2: Wait for sweep completion
    wait_sweep = BashSensor(
        task_id='wait_sweep',
        bash_command='spawn status $(cat /tmp/sweep_id.txt) --check-complete',
        poke_interval=60,  # Check every 60 seconds
        timeout=7200,  # 2 hour timeout
        mode='poke',
    )

    # Task 3: Verify completion
    def verify_completion(**context):
        """Verify sweep completed successfully."""
        with open('/tmp/sweep_id.txt') as f:
            sweep_id = f.read().strip()

        result = subprocess.run(
            ['spawn', 'status', sweep_id, '--json'],
            capture_output=True,
            text=True,
            check=True
        )

        import json
        status = json.loads(result.stdout)

        if status['Status'] != 'COMPLETED':
            raise Exception(f"Sweep not completed: {status['Status']}")

        print(f"Sweep {sweep_id} completed successfully!")
        print(f"Total instances launched: {status['Launched']}")
        print(f"Failed instances: {status['Failed']}")

        return sweep_id

    verify = PythonOperator(
        task_id='verify_completion',
        python_callable=verify_completion,
        provide_context=True,
    )

    # Task 4: Process results
    process_results = BashOperator(
        task_id='process_results',
        bash_command='python /opt/airflow/scripts/process_sweep_results.py',
    )

    # Task 5: Cleanup
    cleanup = BashOperator(
        task_id='cleanup',
        bash_command='rm -f /tmp/sweep_id.txt',
        trigger_rule='all_done',
    )

    # Define task dependencies
    launch_sweep >> wait_sweep >> verify >> process_results >> cleanup
