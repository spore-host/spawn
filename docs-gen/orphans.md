## `spawn orphans`

Report spawn-managed resources that appear orphaned — present but with no
running instance using them:

  - EBS volumes in the 'available' state
  - security groups not attached to any instance
  - the shared infrastructure (key pair, IAM role) when no instances remain
  - Elastic IPs that are unassociated, or attached to a stopped instance
    (an EIP keeps billing even while the instance is stopped)

This is a read-only report. Use 'spawn cleanup' to remove anything — except
Elastic IPs: spawn never allocates them, so it never releases them. Any EIP
listed is yours to release with 'aws ec2 release-address'.

```
spawn orphans [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-regions` |  | bool |  | Search every enabled region |
| `--all` |  | bool |  | Include resources created by other principals (default: only yours) |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |

