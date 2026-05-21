# Common Workflow Language (CWL) Integration Example

## Prerequisites

```bash
pip install cwltool
```

## Running

```bash
# Create input file
cat > inputs.yml <<EOF
params_file:
  class: File
  path: sweep.yaml
wait_timeout: "2h"
EOF

# Run tool
cwltool spawn_tool.cwl inputs.yml
```

## See Also

- [CWL Documentation](https://www.commonwl.org/)
- [spawn WORKFLOW_INTEGRATION.md](../../../WORKFLOW_INTEGRATION.md)
