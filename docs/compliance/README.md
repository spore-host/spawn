# Compliance Documentation

## Overview

This directory contains compliance guides for NIST 800-171 Rev 3, NIST 800-53 Rev 5, and FedRAMP authorization.

## Quick Navigation

### Getting Started
- **[NIST 800-171 Quickstart](nist-800-171-quickstart.md)** - Start here for basic CUI protection
- **[NIST 800-53 Baselines Guide](nist-800-53-baselines.md)** - Low/Moderate/High baseline implementation

### Implementation Guides
- **[Self-Hosted Infrastructure Guide](../how-to/self-hosted-infrastructure.md)** - Deploy customer-owned infrastructure
- **[Control Matrix](control-matrix.md)** - Detailed control implementation mapping

### Reference
- **[Audit Evidence Guide](audit-evidence.md)** - Compliance verification and audits

## Compliance Modes

spawn supports the following compliance modes:

| Mode | Flag | Use Case | Infrastructure | Customer KMS |
|------|------|----------|----------------|--------------|
| **NIST 800-171** | `--nist-800-171` | CUI protection, federal contracts | Shared (warned) | Optional |
| **NIST 800-53 Low** | `--nist-800-53=low` | Low-impact systems | Shared (warned) | Optional |
| **NIST 800-53 Moderate** | `--nist-800-53=moderate` | Moderate-impact systems | Self-hosted required | Recommended |
| **NIST 800-53 High** | `--nist-800-53=high` | High-impact systems | Self-hosted required | Required |
| **FedRAMP Low** | `--fedramp-low` | FedRAMP Low SaaS | Shared (warned) | Optional |
| **FedRAMP Moderate** | `--fedramp-moderate` | FedRAMP Moderate SaaS | Self-hosted required | Recommended |
| **FedRAMP High** | `--fedramp-high` | FedRAMP High SaaS | Self-hosted required | Required |

## Quick Start Examples

### NIST 800-171 (Basic Compliance)

```bash
spawn launch \
  --instance-type t3.micro \
  --nist-800-171 \
  --region us-east-1
```

Automatically enforces:
- EBS encryption
- IMDSv2
- Configuration validation

### NIST 800-53 Moderate (Production)

```bash
# 1. Deploy self-hosted infrastructure (one-time)
spawn config init --self-hosted
aws cloudformation create-stack --stack-name spawn-moderate ...

# 2. Launch compliant instances
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=moderate \
  --subnet-id subnet-private123 \
  --security-group-ids sg-strict456
```

### NIST 800-53 High (Mission-Critical)

```bash
# Create customer KMS key
KEY_ID=$(aws kms create-key \
  --description "spawn High baseline key" \
  --query 'KeyMetadata.KeyId' \
  --output text)

# Launch with High baseline
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=high \
  --subnet-id subnet-private123 \
  --security-group-ids sg-deny-by-default \
  --ebs-kms-key-id $KEY_ID
```

## Validation

Validate compliance at any time:

```bash
# Validate all running instances
spawn validate --nist-800-171

# Validate specific baseline
spawn validate --nist-800-53=moderate

# Output as JSON for automation
spawn validate --nist-800-53=high --output json > compliance-report.json

# Validate infrastructure resources
spawn validate --infrastructure
```

## Documentation Structure

```
docs/
├── compliance/
│   ├── README.md (this file)
│   ├── nist-800-171-quickstart.md
│   ├── nist-800-53-baselines.md
│   ├── control-matrix.md
│   └── audit-evidence.md
└── how-to/
    └── self-hosted-infrastructure.md
```

## Support and Resources

### Internal Documentation
- Control implementation: `pkg/compliance/*.go`
- Test coverage: `pkg/compliance/*_test.go` (86.5% coverage)
- Configuration: `pkg/config/compliance.go`

### External Resources
- [NIST 800-171 Rev 3](https://csrc.nist.gov/publications/detail/sp/800-171/rev-3/final)
- [NIST 800-53 Rev 5](https://csrc.nist.gov/publications/detail/sp/800-53/rev-5/final)
- [FedRAMP](https://www.fedramp.gov/)

### Get Help
- GitHub Issues: https://github.com/spore-host/spore-host/issues
- Command help: `spawn validate --help`
- Config help: `spawn config --help`

## Version

Documentation for spawn v0.14.0 (January 2026)
