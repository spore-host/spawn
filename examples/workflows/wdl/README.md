# Workflow Description Language (WDL) Integration Example

## Prerequisites

```bash
# Install Cromwell
wget https://github.com/broadinstitute/cromwell/releases/latest/download/cromwell.jar
```

## Running

```bash
# Create inputs file
cat > inputs.json <<EOF
{
  "spawn_parameter_sweep.sweep_config": "/path/to/sweep.yaml",
  "spawn_parameter_sweep.timeout": "2h"
}
EOF

# Run with Cromwell
java -jar cromwell.jar run spawn_task.wdl --inputs inputs.json
```

## See Also

- [WDL Documentation](https://openwdl.org/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
