# spawn validate

Validate spawn instances and configuration against compliance controls and infrastructure requirements.

## Synopsis

```bash
spawn validate [flags]
```

## Description

`spawn validate` checks running instances, launch configurations, and infrastructure resources against compliance frameworks (NIST 800-171, NIST 800-53) and verifies that required AWS infrastructure (DynamoDB, S3, Lambda, CloudWatch) is correctly provisioned.

## Flags

#### --nist-800-171
**Type:** String (unused value, presence is the signal)
**Description:** Validate all running instances against NIST 800-171 controls.

```bash
spawn validate --nist-800-171
```

#### --nist-800-53
**Type:** String
**Allowed Values:** `low`, `moderate`, `high`
**Description:** Validate against NIST 800-53 at the specified impact level.

```bash
spawn validate --nist-800-53 moderate
```

#### --infrastructure
**Type:** Boolean
**Default:** `false`
**Description:** Validate that required infrastructure resources (DynamoDB tables, S3 buckets, Lambda functions, CloudWatch alarms) exist and are correctly configured.

```bash
spawn validate --infrastructure
```

#### --instance-id
**Type:** String
**Default:** All instances
**Description:** Validate a specific instance by ID instead of all instances.

```bash
spawn validate --nist-800-171 --instance-id i-0123456789abcdef0
```

#### --region
**Type:** String
**Default:** All regions
**Description:** Limit validation to a specific AWS region.

```bash
spawn validate --nist-800-171 --region us-east-1
```

#### --output
**Type:** String
**Allowed Values:** `text`, `json`
**Default:** `text`
**Description:** Output format.

```bash
spawn validate --nist-800-171 --output json
```

## Examples

```bash
# Validate all instances against NIST 800-171
spawn validate --nist-800-171

# Validate infrastructure resources
spawn validate --infrastructure

# Validate a specific instance at NIST 800-53 moderate
spawn validate --nist-800-53 moderate --instance-id i-0abc123

# JSON output for CI integration
spawn validate --nist-800-171 --output json | jq '.failures'
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | All controls pass |
| 1 | One or more controls failed |
| 2 | Invalid flags or configuration error |

## See Also

- [Compliance documentation](../../compliance/) — Full NIST control matrix
- [spawn launch](launch.md) — Launch with compliance mode (`--compliance`)
