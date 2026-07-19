## `spawn create-ami`

> **Deprecated:** use 'spawn ami create' instead

Create an AMI from a running instance with automatic tagging.

The AMI will be tagged with spawn metadata for easy discovery and management.

Examples:
  # Create AMI from instance
  spawn create-ami my-instance --name pytorch-2.2-cuda12

  # With custom tags
  spawn create-ami i-abc123 \
    --name my-stack-v1.0 \
    --description "My custom software stack" \
    --tag stack=myapp \
    --tag version=1.0 \
    --tag gpu=true

  # Wait for AMI to be available
  spawn create-ami my-instance --name my-ami --wait

  # Allow reboot (default is no-reboot)
  spawn create-ami my-instance --name my-ami --reboot

```
spawn create-ami <instance-id-or-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--description` |  | string |  | Description for the AMI |
| `--name` |  | string |  | Name for the AMI (required) |
| `--reboot` |  | bool |  | Reboot instance before creating AMI (default: no-reboot) |
| `--tag` |  | stringArray |  | Tags in key=value format (can be specified multiple times) |
| `--wait` |  | bool |  | Wait for AMI to become available |

