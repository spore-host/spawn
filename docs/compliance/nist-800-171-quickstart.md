# NIST 800-171 Rev 3 Quickstart Guide

## Overview

NIST 800-171 Rev 3 "Protecting Controlled Unclassified Information in Nonfederal Systems and Organizations" establishes security requirements for protecting Controlled Unclassified Information (CUI) when it resides in nonfederal systems.

**Who needs this:** Organizations that handle CUI for federal contracts, grants, or partnerships.

**What spawn provides:** Technical control implementation for 10 of 110 NIST 800-171 requirements. Organizational controls (policies, procedures, training) remain your responsibility.

## Quick Start

### 1. Enable NIST 800-171 Compliance Mode

Launch instances with automatic compliance enforcement:

```bash
spawn launch \
  --instance-type t3.micro \
  --nist-800-171 \
  --region us-east-1
```

The `--nist-800-171` flag automatically:
- Enables EBS encryption (SC-28)
- Enforces IMDSv2 (AC-17)
- Validates configuration before launch
- Tags instances with compliance metadata

### 2. Validate Before Launch

Check compliance without launching:

```bash
spawn validate --nist-800-171 --config myconfig.yaml
```

### 3. Audit Running Instances

Scan all spawn-managed instances:

```bash
spawn validate --nist-800-171
```

Output shows:
- Compliant instances
- Non-compliant instances with violation details
- Remediation recommendations

## Technical Controls Implemented

spawn implements these NIST 800-171 requirements:

| Control | Requirement | Implementation |
|---------|-------------|----------------|
| **SC-28** | Protection of Information at Rest | EBS encryption enforced |
| **AC-17** | Remote Access | IMDSv2 enforced (no IMDSv1 fallback) |
| **AC-06** | Least Privilege | IAM role scoping (v0.13.0) |
| **AU-02** | Audit Events | Structured audit logging (v0.13.0) |
| **IA-02** | Identification and Authentication | AWS IAM authentication |
| **IA-05** | Authenticator Management | KMS secrets encryption (v0.13.0) |
| **SC-07** | Boundary Protection | Security group configuration |
| **SC-08** | Transmission Confidentiality | TLS for all API calls (AWS SDK) |
| **SC-12** | Cryptographic Key Establishment | KMS integration (v0.13.0) |
| **SC-13** | Cryptographic Protection | FIPS-validated cryptography |

## Configuration Examples

### Basic Launch (Shared Infrastructure)

```bash
# Default: uses shared spore-host-infra infrastructure
spawn launch \
  --instance-type t3.micro \
  --nist-800-171 \
  --ttl 4h
```

**Warning:** Using shared infrastructure with compliance mode generates warnings. For full compliance, use self-hosted infrastructure.

### Self-Hosted Infrastructure (Recommended)

```bash
# 1. Configure self-hosted mode
spawn config init --self-hosted

# 2. Deploy infrastructure (one-time)
cd deployment/cloudformation
aws cloudformation create-stack \
  --stack-name spawn-self-hosted \
  --template-body file://self-hosted-stack.yaml \
  --capabilities CAPABILITY_IAM

# 3. Launch with self-hosted infrastructure
spawn launch \
  --instance-type t3.micro \
  --nist-800-171 \
  --ttl 4h
```

### Customer-Managed KMS Keys (Enhanced Security)

```bash
# Create customer-managed KMS key
KEY_ID=$(aws kms create-key \
  --description "spawn NIST 800-171 encryption key" \
  --query 'KeyMetadata.KeyId' \
  --output text)

# Launch with customer key
spawn launch \
  --instance-type t3.micro \
  --nist-800-171 \
  --ebs-kms-key-id $KEY_ID
```

### Private Subnet Deployment

```bash
# Launch in private subnet (no public IP)
spawn launch \
  --instance-type t3.micro \
  --nist-800-171 \
  --subnet-id subnet-0abc123 \
  --security-group-ids sg-0def456
```

## Validation Reports

### Text Output (Default)

```bash
spawn validate --nist-800-171
```

Example output:

```
Compliance Validation Report (NIST 800-171 Rev 3)
==================================================

Instances Scanned: 12
Compliant: 10
Non-Compliant: 2

Non-Compliant Instances:
  i-0abc123 (my-instance):
    ✗ EBS volumes not encrypted (SC-28)
    ✗ IMDSv2 not enforced (AC-17)

Recommendations:
  1. Terminate and relaunch with --nist-800-171
  2. Enable default EBS encryption: aws ec2 enable-ebs-encryption-by-default
```

### JSON Output (Automation)

```bash
spawn validate --nist-800-171 --output json > compliance-report.json
```

Use for:
- CI/CD pipeline integration
- Automated compliance dashboards
- Historical tracking

## Common Issues and Solutions

### Issue: "EBS encryption required"

**Problem:** Launching without EBS encryption enabled.

**Solution:**
```bash
# Option 1: Use --nist-800-171 flag (automatic)
spawn launch --instance-type t3.micro --nist-800-171

# Option 2: Enable account-wide default
aws ec2 enable-ebs-encryption-by-default --region us-east-1

# Option 3: Explicit flag
spawn launch --instance-type t3.micro --ebs-encrypted
```

### Issue: "IMDSv2 required"

**Problem:** Instance launched with IMDSv1 allowed.

**Solution:** Use `--nist-800-171` flag to automatically enforce IMDSv2.

### Issue: "Self-hosted infrastructure recommended"

**Problem:** Using shared infrastructure with compliance mode.

**Solution:**
```bash
# Deploy self-hosted infrastructure
spawn config init --self-hosted
# Follow prompts to configure customer-owned resources
```

## Strict Mode

Strict mode treats warnings as errors:

```bash
# Set via environment variable
export SPAWN_COMPLIANCE_STRICT_MODE=true

# Or via config file
cat > ~/.spawn/config.yaml <<EOF
compliance:
  mode: nist-800-171
  strict_mode: true
EOF
```

**Effect:** Launch fails if ANY compliance issues detected (including warnings).

**Use case:** Prevent accidental non-compliant launches in production.

## Configuration File

Create `~/.spawn/config.yaml`:

```yaml
compliance:
  mode: nist-800-171
  enforce_encrypted_ebs: true
  enforce_imdsv2: true
  strict_mode: false
  allow_shared_infrastructure: false

infrastructure:
  mode: shared  # or "self-hosted"
```

## Environment Variables

Override settings without editing config:

```bash
# Enable compliance mode
export SPAWN_COMPLIANCE_MODE=nist-800-171

# Enable strict mode
export SPAWN_COMPLIANCE_STRICT_MODE=true

# Enforce specific controls
export SPAWN_COMPLIANCE_ENFORCE_ENCRYPTED_EBS=true
export SPAWN_COMPLIANCE_ENFORCE_IMDSV2=true
```

## Customer Responsibilities

spawn implements **technical controls only**. You are responsible for:

### Organizational Controls (100 of 110 requirements)
- Security policies and procedures
- Personnel security (background checks, training)
- Physical security
- Incident response procedures
- Configuration management
- Risk assessments
- Continuous monitoring processes
- Third-party assessments

### Documentation
- System Security Plan (SSP)
- Policies and procedures
- Training records
- Audit logs retention
- Incident response playbooks

### Ongoing Operations
- Security awareness training
- Access control reviews
- Vulnerability management
- Patch management
- Security audits

## FedRAMP Pathway

NIST 800-171 is a subset of NIST 800-53 Low baseline. If pursuing FedRAMP authorization:

1. **Start with NIST 800-171** - Implement technical controls
2. **Upgrade to NIST 800-53 Low** - `--nist-800-53=low`
3. **Deploy self-hosted infrastructure** - Required for FedRAMP
4. **Contract 3PAO** - Third-party assessment organization
5. **Prepare SSP** - System Security Plan
6. **FedRAMP authorization** - Agency or PMO authorization

See [FedRAMP documentation](fedramp-quickstart.md) for details.

## Additional Resources

- [NIST 800-171 Rev 3 Full Text](https://csrc.nist.gov/publications/detail/sp/800-171/rev-3/final)
- [NIST 800-53 Baselines Guide](nist-800-53-baselines.md)
- [Self-Hosted Infrastructure Guide](../how-to/self-hosted-infrastructure.md)
- [Control Matrix](control-matrix.md)

## Support

Questions or issues:
- GitHub Issues: https://github.com/spore-host/spore-host/issues
- Command help: `spawn validate --help`
- Config help: `spawn config --help`
