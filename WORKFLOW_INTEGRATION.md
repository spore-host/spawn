# Workflow Orchestration Integration Guide

## Overview

spawn provides CLI-first workflow integration designed to work seamlessly with popular orchestration tools. Rather than requiring tight integrations or custom plugins, spawn exposes three powerful flags that make it easy to use from any workflow system that can execute shell commands.

## Core Integration Features

### 1. Asynchronous Execution (`--detach`)

Launch parameter sweeps in AWS Lambda and return immediately, allowing your workflow to continue without maintaining a persistent connection.

```bash
spawn launch --params sweep.yaml --detach
```

**How it works:**
- Sweep orchestration runs in Lambda (spore-host-infra account)
- Your workflow continues immediately
- Sweep survives disconnects and workflow restarts
- Perfect for long-running sweeps

### 2. Output Capture (`--output-id`)

Write sweep/instance IDs to files for easy capture and use in subsequent workflow steps.

```bash
spawn launch --params sweep.yaml --detach --output-id /tmp/sweep_id.txt
SWEEP_ID=$(cat /tmp/sweep_id.txt)
```

**Supported for:**
- Parameter sweeps (sweep ID)
- Job arrays (array ID)
- Single instances (instance ID)
- Batch queue instances (instance ID)

### 3. Synchronous Waiting (`--wait`)

Wait for sweep completion with automatic polling, simplifying workflow logic.

```bash
spawn launch --params sweep.yaml --detach --wait --wait-timeout 2h
```

**Features:**
- Automatic status polling (30-second intervals)
- Optional timeout support
- Progress updates to stderr
- Exit code indicates success/failure

### 4. Completion Checking (`--check-complete`)

Simple completion check with standardized exit codes for workflow branching.

```bash
spawn status $SWEEP_ID --check-complete
# Exit codes: 0=complete, 1=failed, 2=running, 3=error
```

**Exit codes:**
- `0`: Sweep completed successfully
- `1`: Sweep failed or was cancelled
- `2`: Sweep still running
- `3`: Error querying status

## Quick Start

### Pattern 1: Synchronous (Simple)

Best for: Workflows that handle long-running tasks naturally

```bash
# Launch and wait (blocking)
spawn launch --params sweep.yaml --detach --wait --wait-timeout 2h

# Check exit code
if [ $? -eq 0 ]; then
    echo "Sweep completed successfully!"
else
    echo "Sweep failed!"
    exit 1
fi
```

### Pattern 2: Asynchronous (Manual Polling)

Best for: Workflows that need fine-grained control

```bash
# Launch detached sweep
spawn launch --params sweep.yaml --detach --output-id /tmp/sweep_id.txt
SWEEP_ID=$(cat /tmp/sweep_id.txt)

# Poll for completion
while true; do
    spawn status $SWEEP_ID --check-complete
    EXIT_CODE=$?

    if [ $EXIT_CODE -eq 0 ]; then
        echo "Sweep completed!"
        break
    elif [ $EXIT_CODE -eq 1 ]; then
        echo "Sweep failed!"
        exit 1
    elif [ $EXIT_CODE -eq 3 ]; then
        echo "Error querying status!"
        exit 1
    fi

    # Still running, wait and check again
    sleep 60
done
```

### Pattern 3: Fire-and-Forget

Best for: Workflows that launch sweeps but don't need to wait

```bash
# Just launch and continue
spawn launch --params sweep.yaml --detach --output-id /tmp/sweep_id.txt

# Optionally log the sweep ID
echo "Launched sweep: $(cat /tmp/sweep_id.txt)"
```

## Workflow Tool Integration

> **Note:** This guide includes detailed examples for the most popular workflow tools. See [`examples/workflows/`](examples/workflows/) for additional examples including Dagster, Luigi, and Temporal.

### Apache Airflow

Airflow is a platform for programmatically authoring, scheduling, and monitoring workflows.

**Installation:**
```bash
pip install apache-airflow
```

**Example DAG:**
```python
from airflow import DAG
from airflow.operators.bash import BashOperator
from airflow.sensors.bash import BashSensor
from datetime import datetime, timedelta

default_args = {
    'owner': 'data-team',
    'depends_on_past': False,
    'start_date': datetime(2024, 1, 1),
    'retries': 1,
    'retry_delay': timedelta(minutes=5),
}

with DAG('spawn_parameter_sweep',
         default_args=default_args,
         schedule_interval='@daily',
         catchup=False) as dag:

    # Launch sweep
    launch_sweep = BashOperator(
        task_id='launch_sweep',
        bash_command='spawn launch --params /opt/airflow/sweeps/daily_analysis.yaml --detach --output-id /tmp/sweep_id.txt'
    )

    # Wait for completion
    wait_sweep = BashSensor(
        task_id='wait_sweep',
        bash_command='spawn status $(cat /tmp/sweep_id.txt) --check-complete',
        poke_interval=60,
        timeout=7200,
        mode='poke'
    )

    # Process results
    process_results = BashOperator(
        task_id='process_results',
        bash_command='python /opt/airflow/scripts/process_sweep_results.py'
    )

    launch_sweep >> wait_sweep >> process_results
```

**Custom Operator (Advanced):**
```python
from airflow.models import BaseOperator
from airflow.utils.decorators import apply_defaults
import subprocess
import time

class SpawnSweepOperator(BaseOperator):
    @apply_defaults
    def __init__(self, params_file, timeout=7200, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.params_file = params_file
        self.timeout = timeout

    def execute(self, context):
        # Launch sweep
        result = subprocess.run(
            ['spawn', 'launch', '--params', self.params_file,
             '--detach', '--output-id', '/tmp/sweep_id.txt'],
            capture_output=True, text=True, check=True
        )

        # Read sweep ID
        with open('/tmp/sweep_id.txt') as f:
            sweep_id = f.read().strip()

        self.log.info(f"Launched sweep: {sweep_id}")

        # Poll for completion
        start_time = time.time()
        while True:
            if time.time() - start_time > self.timeout:
                raise Exception(f"Sweep timeout after {self.timeout}s")

            result = subprocess.run(
                ['spawn', 'status', sweep_id, '--check-complete'],
                capture_output=True
            )

            if result.returncode == 0:
                self.log.info("Sweep completed successfully!")
                return sweep_id
            elif result.returncode == 1:
                raise Exception("Sweep failed!")
            elif result.returncode == 3:
                raise Exception("Error querying sweep status!")

            time.sleep(60)
```

### Prefect

Prefect is a modern workflow orchestration platform with dynamic task generation and powerful error handling.

**Installation:**
```bash
pip install prefect
```

**Example Flow:**
```python
from prefect import flow, task
import subprocess
import time

@task(retries=3, retry_delay_seconds=60)
def launch_spawn_sweep(params_file: str) -> str:
    """Launch a spawn parameter sweep."""
    result = subprocess.run(
        ['spawn', 'launch', '--params', params_file,
         '--detach', '--output-id', '/tmp/sweep_id.txt'],
        capture_output=True, text=True, check=True
    )

    with open('/tmp/sweep_id.txt') as f:
        sweep_id = f.read().strip()

    print(f"Launched sweep: {sweep_id}")
    return sweep_id

@task(timeout_seconds=7200)
def wait_for_sweep(sweep_id: str):
    """Wait for sweep to complete."""
    while True:
        result = subprocess.run(
            ['spawn', 'status', sweep_id, '--check-complete'],
            capture_output=True
        )

        if result.returncode == 0:
            print("Sweep completed!")
            return
        elif result.returncode == 1:
            raise Exception("Sweep failed!")
        elif result.returncode == 3:
            raise Exception("Error querying status!")

        time.sleep(60)

@task
def process_results():
    """Process sweep results."""
    print("Processing results...")
    # Add your processing logic here

@flow(name="spawn-parameter-sweep")
def spawn_sweep_flow(params_file: str):
    """Complete workflow for spawn parameter sweep."""
    sweep_id = launch_spawn_sweep(params_file)
    wait_for_sweep(sweep_id)
    process_results()

if __name__ == "__main__":
    spawn_sweep_flow("/path/to/sweep.yaml")
```

### Nextflow

Nextflow is a workflow system for computational pipelines, popular in bioinformatics.

**Example Workflow:**
```nextflow
#!/usr/bin/env nextflow

params.sweep_file = "sweep.yaml"
params.sweep_timeout = "2h"

process launchSweep {
    output:
    env SWEEP_ID into sweep_channel

    script:
    """
    spawn launch --params ${params.sweep_file} --detach --output-id sweep_id.txt
    export SWEEP_ID=\$(cat sweep_id.txt)
    """
}

process waitSweep {
    input:
    env SWEEP_ID from sweep_channel

    script:
    """
    spawn launch --params ${params.sweep_file} --detach --wait --wait-timeout ${params.sweep_timeout}
    """
}

process processResults {
    input:
    env SWEEP_ID from sweep_channel

    script:
    """
    echo "Processing results from sweep \${SWEEP_ID}"
    # Add processing logic
    """
}

workflow {
    launchSweep | waitSweep | processResults
}
```

### Snakemake

Snakemake is a workflow management system for reproducible and scalable data analysis.

**Example Snakefile:**
```python
SWEEP_FILE = "config/sweep.yaml"

rule all:
    input:
        "results/sweep_complete.txt"

rule launch_sweep:
    output:
        sweep_id="sweep_id.txt"
    shell:
        "spawn launch --params {SWEEP_FILE} --detach --output-id {output.sweep_id}"

rule wait_sweep:
    input:
        sweep_id="sweep_id.txt"
    output:
        "results/sweep_complete.txt"
    shell:
        """
        SWEEP_ID=$(cat {input.sweep_id})
        spawn launch --params {SWEEP_FILE} --detach --wait --wait-timeout 2h
        echo "Completed: $SWEEP_ID" > {output}
        """

rule process_results:
    input:
        "results/sweep_complete.txt"
    output:
        "results/analysis.txt"
    shell:
        "python scripts/analyze_results.py > {output}"
```

### AWS Step Functions

Step Functions is AWS's serverless workflow orchestration service.

**State Machine Definition:**
```json
{
  "Comment": "Spawn Parameter Sweep Workflow",
  "StartAt": "LaunchSweep",
  "States": {
    "LaunchSweep": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:ACCOUNT:function:spawn-launcher",
      "ResultPath": "$.sweepId",
      "Next": "WaitForSweep"
    },
    "WaitForSweep": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:ACCOUNT:function:spawn-status-checker",
      "ResultPath": "$.status",
      "Next": "CheckStatus"
    },
    "CheckStatus": {
      "Type": "Choice",
      "Choices": [
        {
          "Variable": "$.status",
          "StringEquals": "COMPLETED",
          "Next": "ProcessResults"
        },
        {
          "Variable": "$.status",
          "StringEquals": "RUNNING",
          "Next": "WaitDelay"
        },
        {
          "Variable": "$.status",
          "StringEquals": "FAILED",
          "Next": "HandleFailure"
        }
      ],
      "Default": "WaitDelay"
    },
    "WaitDelay": {
      "Type": "Wait",
      "Seconds": 60,
      "Next": "WaitForSweep"
    },
    "ProcessResults": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:ACCOUNT:function:process-results",
      "End": true
    },
    "HandleFailure": {
      "Type": "Fail",
      "Error": "SweepFailed",
      "Cause": "Parameter sweep failed"
    }
  }
}
```

**Lambda Functions:**

`spawn-launcher` (Python):
```python
import boto3
import subprocess

def handler(event, context):
    params_file = event['params_file']

    result = subprocess.run(
        ['spawn', 'launch', '--params', params_file,
         '--detach', '--output-id', '/tmp/sweep_id.txt'],
        capture_output=True, text=True
    )

    with open('/tmp/sweep_id.txt') as f:
        sweep_id = f.read().strip()

    return {'sweepId': sweep_id}
```

`spawn-status-checker` (Python):
```python
import subprocess

def handler(event, context):
    sweep_id = event['sweepId']

    result = subprocess.run(
        ['spawn', 'status', sweep_id, '--check-complete'],
        capture_output=True
    )

    if result.returncode == 0:
        return {'status': 'COMPLETED'}
    elif result.returncode == 1:
        return {'status': 'FAILED'}
    elif result.returncode == 2:
        return {'status': 'RUNNING'}
    else:
        return {'status': 'ERROR'}
```

### Argo Workflows

Argo is a Kubernetes-native workflow engine for orchestrating parallel jobs.

**Example Workflow:**
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  generateName: spawn-sweep-
spec:
  entrypoint: spawn-sweep

  volumes:
  - name: aws-credentials
    secret:
      secretName: aws-credentials

  templates:
  - name: spawn-sweep
    steps:
    - - name: launch
        template: launch-sweep
    - - name: wait
        template: wait-sweep
        arguments:
          parameters:
          - name: sweep-id
            value: "{{steps.launch.outputs.result}}"
    - - name: process
        template: process-results

  - name: launch-sweep
    container:
      image: scttfrdmn/spawn:latest
      command: [sh, -c]
      args:
      - |
        spawn launch --params /config/sweep.yaml --detach --output-id /tmp/sweep_id.txt
        cat /tmp/sweep_id.txt
      volumeMounts:
      - name: aws-credentials
        mountPath: /root/.aws
        readOnly: true

  - name: wait-sweep
    inputs:
      parameters:
      - name: sweep-id
    container:
      image: scttfrdmn/spawn:latest
      command: [sh, -c]
      args:
      - |
        while true; do
          spawn status {{inputs.parameters.sweep-id}} --check-complete
          EXIT_CODE=$?
          if [ $EXIT_CODE -eq 0 ]; then
            echo "Sweep completed!"
            exit 0
          elif [ $EXIT_CODE -eq 1 ]; then
            echo "Sweep failed!"
            exit 1
          fi
          sleep 60
        done
      volumeMounts:
      - name: aws-credentials
        mountPath: /root/.aws
        readOnly: true

  - name: process-results
    container:
      image: python:3.11
      command: [python]
      args: ["/scripts/process.py"]
```

### Common Workflow Language (CWL)

CWL is a specification for describing command-line tools and workflows.

**Tool Definition (`spawn-tool.cwl`):**
```yaml
cwlVersion: v1.2
class: CommandLineTool

baseCommand: [spawn, launch]

inputs:
  params_file:
    type: File
    inputBinding:
      prefix: --params

  detach:
    type: boolean
    default: true
    inputBinding:
      prefix: --detach

  output_id_file:
    type: string
    default: "sweep_id.txt"
    inputBinding:
      prefix: --output-id

  wait:
    type: boolean
    default: false
    inputBinding:
      prefix: --wait

  wait_timeout:
    type: string?
    inputBinding:
      prefix: --wait-timeout

outputs:
  sweep_id:
    type: File
    outputBinding:
      glob: $(inputs.output_id_file)

requirements:
  DockerRequirement:
    dockerPull: scttfrdmn/spawn:latest
```

**Workflow Definition (`spawn-workflow.cwl`):**
```yaml
cwlVersion: v1.2
class: Workflow

inputs:
  sweep_config: File

outputs:
  results:
    type: File
    outputSource: process_results/output

steps:
  launch_sweep:
    run: spawn-tool.cwl
    in:
      params_file: sweep_config
      detach: true
      wait: true
      wait_timeout: "2h"
    out: [sweep_id]

  process_results:
    run: process-tool.cwl
    in:
      sweep_id: launch_sweep/sweep_id
    out: [output]
```

### Workflow Description Language (WDL)

WDL is a workflow language developed for genomic analysis pipelines.

**Task Definition:**
```wdl
version 1.0

task spawn_sweep {
  input {
    File params_file
    String wait_timeout = "2h"
  }

  command <<<
    spawn launch --params ~{params_file} --detach --wait --wait-timeout ~{wait_timeout} --output-id sweep_id.txt
    cat sweep_id.txt
  >>>

  output {
    String sweep_id = read_string("sweep_id.txt")
  }

  runtime {
    docker: "scttfrdmn/spawn:latest"
    memory: "4 GB"
    cpu: 2
  }
}

task process_results {
  input {
    String sweep_id
  }

  command <<<
    echo "Processing sweep: ~{sweep_id}"
    # Add processing logic
  >>>

  output {
    File results = stdout()
  }

  runtime {
    docker: "python:3.11"
    memory: "8 GB"
    cpu: 4
  }
}

workflow spawn_parameter_sweep {
  input {
    File sweep_config
  }

  call spawn_sweep {
    input:
      params_file = sweep_config
  }

  call process_results {
    input:
      sweep_id = spawn_sweep.sweep_id
  }

  output {
    File final_results = process_results.results
  }
}
```

## Advanced Patterns

### Parallel Sweeps

Launch multiple sweeps in parallel:

```bash
# Launch sweep A
spawn launch --params sweep_a.yaml --detach --output-id /tmp/sweep_a_id.txt &
PID_A=$!

# Launch sweep B
spawn launch --params sweep_b.yaml --detach --output-id /tmp/sweep_b_id.txt &
PID_B=$!

# Wait for both to finish launching
wait $PID_A
wait $PID_B

# Now wait for completion
SWEEP_A=$(cat /tmp/sweep_a_id.txt)
SWEEP_B=$(cat /tmp/sweep_b_id.txt)

# Poll both
while true; do
    spawn status $SWEEP_A --check-complete
    A_STATUS=$?

    spawn status $SWEEP_B --check-complete
    B_STATUS=$?

    if [ $A_STATUS -eq 0 ] && [ $B_STATUS -eq 0 ]; then
        echo "Both sweeps completed!"
        break
    elif [ $A_STATUS -eq 1 ] || [ $B_STATUS -eq 1 ]; then
        echo "One or more sweeps failed!"
        exit 1
    fi

    sleep 60
done
```

### Conditional Execution

Run different sweeps based on conditions:

```bash
# Run initial sweep
spawn launch --params initial_sweep.yaml --detach --wait

# Check if we should run follow-up
if [ -f /tmp/needs_followup ]; then
    echo "Running follow-up sweep..."
    spawn launch --params followup_sweep.yaml --detach --wait
fi
```

### Error Recovery

Automatically retry failed sweeps:

```bash
MAX_RETRIES=3
RETRY_COUNT=0

while [ $RETRY_COUNT -lt $MAX_RETRIES ]; do
    spawn launch --params sweep.yaml --detach --wait

    if [ $? -eq 0 ]; then
        echo "Sweep completed successfully!"
        exit 0
    else
        RETRY_COUNT=$((RETRY_COUNT + 1))
        echo "Sweep failed, retry $RETRY_COUNT/$MAX_RETRIES..."
        sleep 300
    fi
done

echo "Sweep failed after $MAX_RETRIES retries!"
exit 1
```

### Chained Sweeps

Run sweeps sequentially with data passing:

```bash
# Stage 1: Data preprocessing
spawn launch --params stage1_preprocess.yaml --detach --wait --output-id /tmp/stage1_id.txt

# Stage 2: Analysis (depends on stage 1)
spawn launch --params stage2_analysis.yaml --detach --wait --output-id /tmp/stage2_id.txt

# Stage 3: Visualization (depends on stage 2)
spawn launch --params stage3_visualize.yaml --detach --wait --output-id /tmp/stage3_id.txt

echo "Pipeline completed!"
```

## Docker Usage

spawn is available as a Docker image for easy integration with containerized workflows.

### Pull Image

```bash
docker pull scttfrdmn/spawn:latest
# or specific version
docker pull scttfrdmn/spawn:v0.12.0
```

### Basic Usage

```bash
docker run -v ~/.aws:/root/.aws \
    -v $(pwd):/workspace \
    -w /workspace \
    scttfrdmn/spawn:latest \
    launch --params sweep.yaml --detach
```

### With Output File

```bash
docker run -v ~/.aws:/root/.aws \
    -v $(pwd):/workspace \
    -w /workspace \
    scttfrdmn/spawn:latest \
    launch --params sweep.yaml --detach --output-id /workspace/sweep_id.txt
```

### In CI/CD

```yaml
# GitHub Actions example
- name: Launch spawn sweep
  run: |
    docker run \
      -v $HOME/.aws:/root/.aws \
      -v $GITHUB_WORKSPACE:/workspace \
      -w /workspace \
      scttfrdmn/spawn:latest \
      launch --params sweep.yaml --detach --wait --wait-timeout 2h
```

## Exit Codes Reference

### launch command

- `0`: Success (launched, or completed if `--wait`)
- `1`: Failure (launch error, or sweep failed if `--wait`)

### status command

**Without `--check-complete`:**
- `0`: Success (status retrieved)
- `1`: Error (failed to query status)

**With `--check-complete`:**
- `0`: Sweep completed successfully
- `1`: Sweep failed or was cancelled
- `2`: Sweep still running
- `3`: Error querying status

## Troubleshooting

### Issue: sweep_id.txt not created

**Cause:** Permission denied or invalid path

**Solution:**
```bash
# Ensure directory exists and is writable
mkdir -p /tmp
chmod 755 /tmp

# Use absolute paths
spawn launch --params sweep.yaml --detach --output-id /tmp/sweep_id.txt
```

### Issue: --wait times out

**Cause:** Sweep takes longer than timeout

**Solution:**
```bash
# Increase timeout
spawn launch --params sweep.yaml --detach --wait --wait-timeout 4h

# Or use manual polling for unlimited wait
spawn launch --params sweep.yaml --detach --output-id /tmp/sweep_id.txt
while true; do
    spawn status $(cat /tmp/sweep_id.txt) --check-complete && break
    sleep 60
done
```

### Issue: Docker container can't access AWS credentials

**Cause:** Credentials not mounted correctly

**Solution:**
```bash
# Mount AWS credentials directory
docker run -v ~/.aws:/root/.aws:ro \
    scttfrdmn/spawn:latest \
    launch --params sweep.yaml --detach

# Or use environment variables
docker run \
    -e AWS_ACCESS_KEY_ID \
    -e AWS_SECRET_ACCESS_KEY \
    -e AWS_SESSION_TOKEN \
    scttfrdmn/spawn:latest \
    launch --params sweep.yaml --detach
```

### Issue: Exit code always 0 in workflow

**Cause:** Not using `--check-complete` flag

**Solution:**
```bash
# Wrong (always returns 0)
spawn status $SWEEP_ID

# Correct (returns meaningful exit codes)
spawn status $SWEEP_ID --check-complete
```

## Best Practices

### 1. Always Use --detach for Sweeps

Lambda orchestration is more reliable than local orchestration for workflows:

```bash
# Good
spawn launch --params sweep.yaml --detach

# Avoid for workflows (unless sweep is very short)
spawn launch --params sweep.yaml  # Requires persistent connection
```

### 2. Capture Output IDs

Always save IDs for tracking and debugging:

```bash
# Good
spawn launch --params sweep.yaml --detach --output-id /tmp/sweep_id.txt
echo "Launched: $(cat /tmp/sweep_id.txt)" >> workflow.log

# Avoid
spawn launch --params sweep.yaml --detach  # ID only in stderr
```

### 3. Set Reasonable Timeouts

Prevent workflows from hanging indefinitely:

```bash
# Good
spawn launch --params sweep.yaml --detach --wait --wait-timeout 2h

# Avoid
spawn launch --params sweep.yaml --detach --wait  # No timeout
```

### 4. Check Exit Codes

Always check exit codes for proper error handling:

```bash
# Good
spawn status $SWEEP_ID --check-complete
if [ $? -ne 0 ]; then
    echo "Sweep not completed successfully!"
    exit 1
fi

# Avoid
spawn status $SWEEP_ID  # Ignores exit code
```

### 5. Use JSON Output for Parsing

When you need to extract specific fields:

```bash
# Get specific status field
spawn status $SWEEP_ID --json | jq -r '.Status'

# Get launched count
spawn status $SWEEP_ID --json | jq -r '.Launched'
```

### 6. Log Everything

Maintain audit trail of workflow execution:

```bash
{
    echo "=== Sweep Launch ==="
    echo "Timestamp: $(date -Iseconds)"
    spawn launch --params sweep.yaml --detach --output-id /tmp/sweep_id.txt
    echo "Sweep ID: $(cat /tmp/sweep_id.txt)"
} >> workflow.log 2>&1
```

## Performance Optimization

### Minimize Status Polling

```bash
# Good (60s intervals)
while true; do
    spawn status $SWEEP_ID --check-complete && break
    sleep 60
done

# Avoid (5s intervals - too frequent)
while true; do
    spawn status $SWEEP_ID --check-complete && break
    sleep 5
done
```

### Parallel Status Checks

```bash
# Check multiple sweeps in parallel
for sweep_id in $SWEEP_IDS; do
    (spawn status $sweep_id --check-complete) &
done
wait
```

### Use --wait for Simplicity

For single sweeps, --wait is simpler than manual polling:

```bash
# Simple
spawn launch --params sweep.yaml --detach --wait --wait-timeout 2h

# Complex (same result)
spawn launch --params sweep.yaml --detach --output-id /tmp/id.txt
while true; do
    spawn status $(cat /tmp/id.txt) --check-complete && break
    sleep 60
done
```

## Support

For issues or questions:
- GitHub: https://github.com/spore-host/spore-host/issues
- Documentation: https://github.com/spore-host/spore-host/tree/main/spawn

## See Also

- [spawn README](README.md) - Main documentation
- [Parameter Sweep Guide](docs/parameter-sweeps.md) - Sweep syntax and options
- [examples/workflows/](examples/workflows/) - Complete workflow examples
