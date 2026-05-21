# spawn pipeline

Manage multi-stage pipelines with DAG dependencies.

## Synopsis

```bash
spawn pipeline <subcommand> [flags]
```

## Description

Pipelines orchestrate complex workflows where each stage runs on separate EC2 instances with different instance types. Stages can depend on each other, forming a directed acyclic graph (DAG). Data passes between stages via S3 (batch mode) or direct network streaming (real-time).

Pipeline definitions are written in YAML and specify stages, instance types, commands, and dependencies.

## Subcommands

| Subcommand | Description |
|------------|-------------|
| `validate <file>` | Validate a pipeline definition file |
| `graph <file>` | Display the pipeline DAG as ASCII art |
| `launch <file>` | Launch a pipeline |
| `status <pipeline-id>` | Show pipeline status |
| `collect <pipeline-id>` | Download pipeline results |
| `list` | List all pipelines |
| `cancel <pipeline-id>` | Cancel a running pipeline |

## Subcommand Details

### validate

Validate a pipeline YAML definition without launching it.

```bash
spawn pipeline validate pipeline.yaml
```

### graph

Display the dependency graph as ASCII art.

```bash
spawn pipeline graph pipeline.yaml
spawn pipeline graph pipeline.yaml --simple     # Simplified view
spawn pipeline graph pipeline.yaml --stats      # Show graph statistics
spawn pipeline graph pipeline.yaml --json       # JSON adjacency list
```

### launch

Launch a pipeline from a YAML definition.

```bash
spawn pipeline launch pipeline.yaml
spawn pipeline launch pipeline.yaml --detached  # Return immediately
spawn pipeline launch pipeline.yaml --wait      # Wait for completion
spawn pipeline launch pipeline.yaml --region us-west-2
```

### status

Show the current status of a running or completed pipeline.

```bash
spawn pipeline status pipeline-abc123
```

### collect

Download results from a completed pipeline.

```bash
spawn pipeline collect pipeline-abc123
spawn pipeline collect pipeline-abc123 --output ./my-results
spawn pipeline collect pipeline-abc123 --stage preprocessing  # Single stage only
```

### list

List all pipelines for the current user.

```bash
spawn pipeline list
spawn pipeline list --status RUNNING
spawn pipeline list --json
```

**`--status` filter values:** `INITIALIZING`, `RUNNING`, `COMPLETED`, `FAILED`, `CANCELLED`

### cancel

Cancel a running pipeline and terminate all instances.

```bash
spawn pipeline cancel pipeline-abc123
```

## Pipeline Definition Format

```yaml
name: my-pipeline
stages:
  - name: preprocess
    instance_type: r6i.2xlarge
    ami: ami-abc123
    command: python preprocess.py
    outputs:
      - s3://my-bucket/preprocessed/

  - name: train
    instance_type: p3.8xlarge
    ami: ami-gpu123
    command: python train.py
    depends_on: [preprocess]
    inputs:
      - s3://my-bucket/preprocessed/
    outputs:
      - s3://my-bucket/model/

  - name: evaluate
    instance_type: m5.xlarge
    ami: ami-abc123
    command: python evaluate.py
    depends_on: [train]
    inputs:
      - s3://my-bucket/model/
```

## See Also

- [spawn launch](launch.md) — Single-instance launch
- [spawn collect](collect-results.md) — Collect results from instances
- [spawn cancel](cancel.md) — Cancel parameter sweeps
