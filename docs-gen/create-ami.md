## `spawn create-ami`

> **Deprecated:** use 'spawn ami create' instead

Create an AMI from a running instance with automatic tagging.

The AMI will be tagged with spawn metadata for easy discovery and management.
It also records where it came from: spawn:image-spawn-version (this spawn's
version) and, when the source instance has them, its spawn:version,
spawn:spored-version, spawn:mpi-enabled and spawn:job-array-index as
spawn:source-version, spawn:source-spored-version, spawn:source-mpi-enabled
and spawn:source-job-array-index. --tag cannot set these. 'spawn ami list'
shows the source version.

Before imaging, spawn removes its --command run state from the instance over
SSM: the completion file, /tmp/SPAWN_* (the exit code), the rendered and
embedded command, and the sweep environment. Otherwise an instance launched
from the image starts with a stale SPAWN_COMPLETE and --on-complete fires at
boot. If that cannot be done (no SSM, the instance is stopped, the command is
still running), the image is not created; --force creates it anyway.

Root's authorized_keys on the source is left as it is. An image of an MPI
cohort member (--mpi) carries the cohort's cluster key there; an instance
launched from the image removes every line ending in spawn-mpi-cohort at its
first boot and keeps every other line. A non-zero rank imaged with spawn before
v0.147.0 also carries rank 0's key without that tag, which cannot be told from
your own keys, so an image like that should be rebuilt: launch from it, delete
the root@ip-... line in /root/.ssh/authorized_keys that is not yours, and image
that instance.

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

