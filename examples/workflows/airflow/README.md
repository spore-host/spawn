# Apache Airflow Integration Example

This example demonstrates how to integrate spawn with Apache Airflow for automated parameter sweep execution.

## Files

- `parameter_sweep_dag.py` - Complete DAG using bash operators
- `operators/spawn_operator.py` - Custom operator for cleaner integration
- `dags/advanced_dag.py` - Advanced example using custom operator

## Prerequisites

```bash
# Install Airflow
pip install apache-airflow

# Ensure spawn is available
which spawn

# Configure AWS credentials
aws configure --profile spore-host-dev
```

## Setup

1. Copy DAG to Airflow directory:
```bash
cp parameter_sweep_dag.py ~/airflow/dags/
```

2. Copy custom operator:
```bash
mkdir -p ~/airflow/plugins/operators
cp operators/spawn_operator.py ~/airflow/plugins/operators/
```

3. Create sweep configuration:
```bash
mkdir -p /opt/airflow/config
cat > /opt/airflow/config/daily_sweep.yaml <<EOF
defaults:
  instance_type: t3.medium
  region: us-east-1

params:
  - name: run-1
    command: "echo 'Processing dataset 1'"
  - name: run-2
    command: "echo 'Processing dataset 2'"
  - name: run-3
    command: "echo 'Processing dataset 3'"
EOF
```

## Usage

### Basic DAG

```python
from airflow import DAG
from airflow.operators.bash import BashOperator

with DAG('spawn_sweep', ...) as dag:
    launch = BashOperator(
        task_id='launch',
        bash_command='spawn launch --params sweep.yaml --detach --output-id /tmp/id.txt'
    )
```

### Custom Operator

```python
from operators.spawn_operator import SpawnSweepOperator

with DAG('spawn_sweep', ...) as dag:
    sweep = SpawnSweepOperator(
        task_id='run_sweep',
        params_file='/opt/airflow/config/sweep.yaml',
        use_wait=True,
        wait_timeout='2h'
    )
```

## Running

1. Start Airflow:
```bash
airflow standalone
```

2. Access UI:
```
http://localhost:8080
```

3. Trigger DAG:
```bash
airflow dags trigger spawn_parameter_sweep
```

4. Monitor:
```bash
airflow dags list-runs -d spawn_parameter_sweep
```

## Troubleshooting

### DAG not appearing

- Check DAG file syntax: `python parameter_sweep_dag.py`
- Check Airflow logs: `tail -f ~/airflow/logs/scheduler/latest/*.log`

### Sweep launch fails

- Verify spawn is in PATH: `which spawn`
- Check AWS credentials: `aws sts get-caller-identity --profile spore-host-dev`
- Check sweep file exists and is valid

### Timeout issues

- Increase timeout in DAG:
  ```python
  wait_sweep = BashSensor(
      task_id='wait_sweep',
      timeout=14400,  # 4 hours
      ...
  )
  ```

- Or use custom operator with longer timeout:
  ```python
  sweep = SpawnSweepOperator(
      task_id='sweep',
      params_file='sweep.yaml',
      timeout=14400
  )
  ```

## Best Practices

1. **Use XCom for sweep IDs**: Pass sweep IDs between tasks
2. **Set appropriate timeouts**: Based on expected sweep duration
3. **Enable email alerts**: For failure notifications
4. **Use SLAs**: Track sweep completion times
5. **Partition sweeps**: Break large sweeps into smaller chunks

## See Also

- [Airflow Documentation](https://airflow.apache.org/docs/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
- [spawn Parameter Sweeps](../../../docs/parameter-sweeps.md)
