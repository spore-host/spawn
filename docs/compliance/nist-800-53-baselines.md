# NIST 800-53 Rev 5 Baselines Guide

## Overview

NIST 800-53 Rev 5 "Security and Privacy Controls for Information Systems and Organizations" provides a comprehensive catalog of security controls for federal information systems. It defines three impact-based baselines: Low, Moderate, and High.

**What spawn provides:** Technical control implementation for 10-20+ controls depending on baseline. Organizational controls remain your responsibility.

## Baseline Comparison

| Baseline | Impact Level | Controls | Use Case | Infrastructure | Timeline |
|----------|--------------|----------|----------|----------------|----------|
| **Low** | Limited adverse effect | 10-13 | Development, test environments | Shared OK | Days |
| **Moderate** | Serious adverse effect | 13-16 | Production systems with sensitive data | Self-hosted required | Weeks |
| **High** | Severe/catastrophic effect | 18-21 | Mission-critical, classified systems | Self-hosted + hardening | Months |

### Progressive Control Enhancement

```
Low Baseline:      NIST 800-171 foundation
                   ↓
Moderate Baseline: Low + private networks + enhanced protection
                   ↓
High Baseline:     Moderate + customer KMS + stringent requirements
```

## Low Baseline

### Overview

**Impact:** Limited adverse effect on organizational operations, assets, or individuals.

**Suitable for:**
- Development and test environments
- Public-facing information systems
- Low-sensitivity data processing

**Key Requirements:**
- EBS encryption (AWS-managed or customer-managed)
- IMDSv2 enforcement
- Security group configuration
- Audit logging
- IAM authentication
- Self-hosted infrastructure (recommended, not required)

### Quick Start

```bash
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=low \
  --region us-east-1
```

### Example Configuration

```bash
# Basic low baseline launch
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=low \
  --ttl 4h \
  --ebs-encrypted

# With optional customer KMS
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=low \
  --ebs-kms-key-id alias/my-key
```

### Validation

```bash
spawn validate --nist-800-53=low
```

## Moderate Baseline

### Overview

**Impact:** Serious adverse effect on organizational operations, assets, or individuals.

**Suitable for:**
- Production systems
- Systems handling PII or sensitive data
- Internal business applications
- FedRAMP Moderate authorization

**Key Requirements (all Low + these):**
- **Self-hosted infrastructure (REQUIRED)**
- Private subnet deployment (no public IPs)
- Customer-managed KMS keys (recommended)
- Enhanced boundary protection
- Flaw remediation processes

### Quick Start

```bash
# 1. Deploy self-hosted infrastructure (one-time)
spawn config init --self-hosted
cd deployment/cloudformation
aws cloudformation create-stack \
  --stack-name spawn-moderate \
  --template-body file://self-hosted-stack.yaml \
  --capabilities CAPABILITY_IAM

# 2. Wait for stack creation
aws cloudformation wait stack-create-complete \
  --stack-name spawn-moderate

# 3. Launch with Moderate baseline
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=moderate \
  --subnet-id subnet-private123 \
  --security-group-ids sg-strict456
```

### Example Configuration

```bash
# Create private VPC setup (one-time)
VPC_ID=$(aws ec2 create-vpc \
  --cidr-block 10.0.0.0/16 \
  --query 'Vpc.VpcId' \
  --output text)

PRIVATE_SUBNET=$(aws ec2 create-subnet \
  --vpc-id $VPC_ID \
  --cidr-block 10.0.1.0/24 \
  --availability-zone us-east-1a \
  --query 'Subnet.SubnetId' \
  --output text)

# Create customer KMS key
KEY_ID=$(aws kms create-key \
  --description "spawn Moderate baseline key" \
  --query 'KeyMetadata.KeyId' \
  --output text)

# Launch with full Moderate compliance
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=moderate \
  --subnet-id $PRIVATE_SUBNET \
  --ebs-kms-key-id $KEY_ID \
  --security-group-ids sg-strict456
```

### Validation

```bash
spawn validate --nist-800-53=moderate --output json
```

### Critical Requirements

⚠️ **IMPORTANT:** Moderate baseline REQUIRES self-hosted infrastructure. Attempts to launch with shared infrastructure will generate errors (or warnings if `allow_shared_infrastructure: true`).

## High Baseline

### Overview

**Impact:** Severe or catastrophic adverse effect on organizational operations, assets, or individuals.

**Suitable for:**
- Mission-critical systems
- Classified information systems
- High-value assets
- FedRAMP High authorization
- National security systems

**Key Requirements (all Moderate + these):**
- **Customer-managed KMS keys (REQUIRED)**
- Explicit security groups (deny-by-default)
- VPC endpoints for AWS services
- Multi-AZ deployment (recommended)
- System backup configuration
- Enhanced monitoring and incident response

### Quick Start

```bash
# 1. Prepare infrastructure
spawn config init --self-hosted
# Deploy CloudFormation stack (includes VPC endpoints)

# 2. Create customer KMS key with strict policy
aws kms create-key \
  --description "spawn High baseline key" \
  --key-policy file://kms-policy.json

# 3. Launch with High baseline
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=high \
  --subnet-id subnet-private123 \
  --security-group-ids sg-deny-by-default \
  --ebs-kms-key-id $KEY_ID
```

### Example Configuration

```bash
# Create deny-by-default security group
SG_ID=$(aws ec2 create-security-group \
  --group-name spawn-high-baseline \
  --description "High baseline deny-by-default" \
  --vpc-id $VPC_ID \
  --query 'GroupId' \
  --output text)

# Add only required egress (explicit allow)
aws ec2 authorize-security-group-egress \
  --group-id $SG_ID \
  --protocol tcp \
  --port 443 \
  --cidr 10.0.0.0/16

# Launch with High baseline
spawn launch \
  --instance-type t3.micro \
  --nist-800-53=high \
  --subnet-id $PRIVATE_SUBNET \
  --security-group-ids $SG_ID \
  --ebs-kms-key-id $KEY_ID \
  --multi-az
```

### Validation

```bash
spawn validate --nist-800-53=high --output json > high-baseline-report.json
```

### Critical Requirements

⚠️ **CRITICAL:** High baseline REQUIRES:
1. Self-hosted infrastructure (mandatory)
2. Customer-managed KMS keys (mandatory)
3. Private subnets (no public IPs)
4. VPC endpoints for AWS services
5. Explicit security group rules

Failure to meet these requirements will block launches in strict mode.

## Control Comparison Table

| Control | Low | Moderate | High |
|---------|-----|----------|------|
| **EBS Encryption** | Required (any KMS) | Required (any KMS) | Required (customer KMS) |
| **IMDSv2** | Required | Required | Required |
| **Private Network** | - | Required | Required |
| **Public IP** | Allowed | Blocked | Blocked |
| **Self-Hosted Infra** | Recommended | Required | Required |
| **Customer KMS** | Optional | Recommended | Required |
| **VPC Endpoints** | - | Recommended | Required |
| **Multi-AZ** | - | - | Recommended |
| **Explicit SG Rules** | - | - | Required |

## Configuration Files

### Low Baseline Config

```yaml
# ~/.spawn/config.yaml
compliance:
  mode: nist-800-53-low
  strict_mode: false
  allow_shared_infrastructure: true

infrastructure:
  mode: shared
```

### Moderate Baseline Config

```yaml
# ~/.spawn/config.yaml
compliance:
  mode: nist-800-53-moderate
  strict_mode: true
  allow_shared_infrastructure: false

infrastructure:
  mode: self-hosted
  dynamodb:
    schedules_table: my-spawn-schedules
  s3:
    binaries_bucket_prefix: my-spawn-binaries
  lambda:
    scheduler_handler_arn: arn:aws:lambda:us-east-1:123456789012:function:my-spawn-scheduler
```

### High Baseline Config

```yaml
# ~/.spawn/config.yaml
compliance:
  mode: nist-800-53-high
  strict_mode: true
  allow_shared_infrastructure: false

infrastructure:
  mode: self-hosted
  # ... (same as Moderate)

# High baseline requires customer KMS
ebs:
  encrypted: true
  kms_key_id: arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012
```

## Validation Reports

### Generate Baseline Report

```bash
# Compare all baselines
spawn validate --compare-baselines

# Output:
# NIST 800-53 Rev 5 Baseline Comparison
# 
# Low Baseline:      10 controls
# Moderate Baseline: 16 controls
# High Baseline:     21 controls
# 
# Progressive Control Enhancement:
# [Table showing control differences]
```

### Automated Validation

```bash
# Daily compliance check
0 0 * * * spawn validate --nist-800-53=moderate --output json >> /var/log/spawn-compliance.log
```

## Migration Between Baselines

### Upgrading from Low to Moderate

```bash
# 1. Deploy self-hosted infrastructure
spawn config init --self-hosted
aws cloudformation create-stack --stack-name spawn-moderate ...

# 2. Update config
cat > ~/.spawn/config.yaml <<EOF
compliance:
  mode: nist-800-53-moderate
infrastructure:
  mode: self-hosted
EOF

# 3. Validate existing instances
spawn validate --nist-800-53=moderate

# 4. Terminate non-compliant instances
# 5. Relaunch with moderate baseline
spawn launch --nist-800-53=moderate ...
```

### Upgrading from Moderate to High

```bash
# 1. Create customer KMS key
KEY_ID=$(aws kms create-key ...)

# 2. Update config
cat > ~/.spawn/config.yaml <<EOF
compliance:
  mode: nist-800-53-high
  strict_mode: true
ebs:
  kms_key_id: $KEY_ID
EOF

# 3. Validate and relaunch
spawn validate --nist-800-53=high
spawn launch --nist-800-53=high --ebs-kms-key-id $KEY_ID ...
```

## FedRAMP Mapping

NIST 800-53 baselines map directly to FedRAMP authorization levels:

- **FedRAMP Low** = NIST 800-53 Low Baseline
- **FedRAMP Moderate** = NIST 800-53 Moderate Baseline
- **FedRAMP High** = NIST 800-53 High Baseline

See [FedRAMP Quickstart Guide](fedramp-quickstart.md) for authorization process details.

## Customer Responsibilities

spawn provides **technical control implementation**. You are responsible for:

### Organizational Controls
- Security policies and procedures
- Personnel security
- Physical security
- Incident response
- Configuration management
- Risk assessments
- Continuous monitoring
- Third-party assessments (if pursuing FedRAMP)

### Documentation
- System Security Plan (SSP)
- Control implementation statements
- Test procedures and results
- Policies and procedures
- Training records

### Ongoing Operations
- Vulnerability scanning
- Penetration testing
- Security audits
- Patch management
- Access reviews
- Incident response

## Additional Resources

- [NIST 800-53 Rev 5 Full Catalog](https://csrc.nist.gov/publications/detail/sp/800-53/rev-5/final)
- [NIST 800-171 Quickstart](nist-800-171-quickstart.md)
- [FedRAMP Quickstart](fedramp-quickstart.md)
- [Self-Hosted Infrastructure Guide](../how-to/self-hosted-infrastructure.md)
- [Control Matrix](control-matrix.md)

## Support

- GitHub Issues: https://github.com/spore-host/spore-host/issues
- Command help: `spawn validate --help`
- Baseline comparison: `spawn validate --compare-baselines`
