## `spawn create-ami`

> **Deprecated:** use 'spawn ami create' instead

Create an AMI from a running instance with automatic tagging.

The AMI will be tagged with spawn metadata for easy discovery and management.

Before imaging, spawn removes its --command run state from the instance over
SSM: the completion file, /tmp/SPAWN_* (the exit code), the rendered and
embedded command, and the sweep environment. Otherwise an instance launched
from the image starts with a stale SPAWN_COMPLETE and --on-complete fires at
boot. If that cannot be done (no SSM, the instance is stopped, the command is
still running), the image is not created; --force creates it anyway.

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
| `--force` |  | bool |  | Create the image even if spawn could not remove its --command run-state files (completion file, exit code, the embedded command) from the instance first; they will be baked into the AMI |
| `--name` |  | string |  | Name for the AMI (required) |
| `--reboot` |  | bool |  | Reboot instance before creating AMI (default: no-reboot) |
| `--tag` |  | stringArray |  | Tags in key=value format (can be specified multiple times) |
| `--wait` |  | bool |  | Wait for AMI to become available |

