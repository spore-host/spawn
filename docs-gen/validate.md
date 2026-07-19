## `spawn validate`

Validate spawn instances and configuration against compliance controls.

This command can validate:
- Running instances against compliance controls (NIST 800-171, NIST 800-53)
- Infrastructure resources (DynamoDB, S3, Lambda, CloudWatch)
- Launch configuration before launching instances

Examples:
  # Validate all running instances against NIST 800-171
  spawn validate --nist-800-171

  # Validate specific instance
  spawn validate --instance-id i-0abc123 --nist-800-171

  # Validate infrastructure resources
  spawn validate --infrastructure

  # Output as JSON for automation
  spawn validate --nist-800-171 -o json

```
spawn validate [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--infrastructure` |  | bool |  | Validate infrastructure resources (DynamoDB, S3, Lambda) |
| `--instance-id` |  | string |  | Specific instance ID to validate |
| `--nist-800-171` |  | string |  | Validate NIST 800-171 compliance |
| `--nist-800-53` |  | string |  | Validate NIST 800-53 compliance (low, moderate, high) |
| `--region` |  | string |  | AWS region to validate (default: all regions) |

