## `spawn list-amis`

> **Deprecated:** use 'spawn ami list' instead

List AMIs created and managed by spawn.

Filters AMIs by spawn tags to show only those created by spawn.
You can filter by stack, version, architecture, and other attributes.

Examples:
  # List all spawn AMIs
  spawn list-amis

  # Filter by stack
  spawn list-amis --stack pytorch

  # Filter by stack and version
  spawn list-amis --stack pytorch --version 2.2

  # Filter by architecture
  spawn list-amis --arch arm64

  # Show only GPU AMIs
  spawn list-amis --gpu true

  # Show deprecated AMIs
  spawn list-amis --deprecated

  # JSON output
  spawn list-amis --json

```
spawn list-amis [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--arch` |  | string |  | Filter by architecture (x86_64 or arm64) |
| `--deprecated` |  | bool |  | Show deprecated AMIs (default: hide deprecated) |
| `--gpu` |  | string |  | Filter by GPU support (true or false) |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--stack` |  | string |  | Filter by stack (spawn:stack tag) |
| `--version` |  | string |  | Filter by version (spawn:version tag) |

