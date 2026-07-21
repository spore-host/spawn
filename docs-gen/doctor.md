## `spawn doctor`

Run a read-only preflight of everything a first launch needs: your tools,
AWS credentials, the resolved account and region, the EC2 and IAM permissions
spawn uses, a usable VPC/subnet, an SSH key, Session Manager, the spored instance
profile, and optional features (reaper backstop, Route 53).

Each check reports pass (✓), warning (⚠, optional feature unavailable), or fail
(✗, a core prerequisite is missing). It launches nothing and changes nothing.

If 'spawn doctor' passes, the Quick Start should work as written. On an
institution-managed AWS account, share the failing IAM checks with your cloud
administrator alongside the IAM baseline (docs: reference/iam-permissions).

Exit codes: 0 = ready (no failures), 1 = a core prerequisite failed.

Examples:
  spawn doctor
  spawn doctor --region us-west-2
  spawn doctor -o json

```
spawn doctor [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region to check (default: your resolved region) |

