## `spawn ami`

Create and list AMIs created by spawn.

Examples:
  spawn ami list
  spawn ami list --stack pytorch --arch arm64
  spawn ami create my-instance --name pytorch-2.4-cuda12

```
spawn ami
```

### `spawn ami create`

Create an AMI from a running instance

```
spawn ami create <instance-id-or-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--description` |  | string |  | Description for the AMI |
| `--name` |  | string |  | Name for the AMI (required) |
| `--reboot` |  | bool |  | Reboot instance before creating AMI (default: no-reboot) |
| `--tag` |  | stringArray |  | Tags in key=value format |
| `--wait` |  | bool |  | Wait for AMI to become available |

### `spawn ami delete`

Deregister a spawn-managed AMI and delete its backing EBS snapshots in one
step. If the AMI was produced by EC2 Image Builder (e.g. 'spawn image import'),
the corresponding Image Builder image resource is also deleted so its
name/version is freed.

This is irreversible. Use 'spawn ami list' to find AMIs.

Examples:
  spawn ami delete ami-0123456789abcdef0
  spawn ami delete ami-0123456789abcdef0 --region us-east-1 --yes

```
spawn ami delete <ami-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn ami list`

List spawn-managed AMIs

```
spawn ami list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--arch` |  | string |  | Filter by architecture (x86_64 or arm64) |
| `--deprecated` |  | bool |  | Show deprecated AMIs |
| `--gpu` |  | string |  | Filter by GPU support (true or false) |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--stack` |  | string |  | Filter by stack (spawn:stack tag) |
| `--version` |  | string |  | Filter by version (spawn:version tag) |

### `spawn ami snapshots`

Show the EBS snapshots that back an AMI, with size, state, and whether each
snapshot is shared with other AMIs (which is why 'spawn ami delete' keeps shared
snapshots instead of deleting them).

Examples:
  spawn ami snapshots ami-0123456789abcdef0
  spawn ami snapshots ami-0123456789abcdef0 -o json

```
spawn ami snapshots <ami-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region (default: current region from AWS config) |

