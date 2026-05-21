# Compliance Control Matrix

## Overview

This document maps NIST 800-171 and NIST 800-53 Rev 5 controls to spawn implementation details. Use this matrix for audits, assessments, and compliance verification.

## Legend

| Symbol | Meaning |
|--------|---------|
| ✅ | Fully implemented by spawn |
| ⚠️ | Partially implemented (customer config required) |
| 📋 | Organizational control (customer responsibility) |
| 🔄 | Planned for future release |

## NIST 800-171 Rev 3 Control Matrix

### Access Control (AC)

| Control | Requirement | spawn Implementation | Evidence Location |
|---------|-------------|---------------------|-------------------|
| **AC-06** | Least Privilege | ✅ IAM role scoping | `pkg/security/iam.go:generateIAMPolicy()` |
| **AC-17** | Remote Access | ✅ IMDSv2 enforcement | `pkg/compliance/nist80171.go:49-58` |

**AC-06 Implementation Details:**
- Scoped IAM policies generated per instance
- Read-only access to spawn infrastructure
- Write access only to instance-specific resources
- Automatic permission revocation on termination

**Code Reference:**
```go
// pkg/security/iam.go
func generateIAMPolicy(instanceID string) *iam.Policy {
    return &iam.Policy{
        Statements: []iam.Statement{
            {
                Effect: "Allow",
                Actions: ["ec2:DescribeInstances"],
                Resources: ["*"],
            },
            {
                Effect: "Allow",
                Actions: ["s3:GetObject"],
                Resources: [fmt.Sprintf("arn:aws:s3:::spawn-binaries-*/%s/*", instanceID)],
            },
        },
    }
}
```

**AC-17 Implementation Details:**
- IMDSv2 tokens required
- No IMDSv1 fallback allowed
- Hop limit set to 1 (single-hop only)
- Enforced via EC2 MetadataOptions

**Code Reference:**
```go
// pkg/compliance/nist80171.go:49-58
Validator: func(cfg *aws.LaunchConfig) error {
    if !cfg.IMDSv2Enforced {
        return errors.New("IMDSv2 required for NIST 800-171 (AC-17)")
    }
    return nil
},
Enforcer: func(cfg *aws.LaunchConfig) {
    cfg.IMDSv2Enforced = true
    cfg.IMDSv2HopLimit = 1
},
```

### Audit and Accountability (AU)

| Control | Requirement | spawn Implementation | Evidence Location |
|---------|-------------|---------------------|-------------------|
| **AU-02** | Event Logging | ✅ Structured audit logging | `pkg/audit/logger.go` |
| **AU-03** | Audit Record Content | ✅ Comprehensive event data | `pkg/audit/event.go:AuditEvent` |
| **AU-12** | Audit Record Generation | ✅ Automatic logging | All `pkg/audit/logger.go:Log*()` calls |

**AU-02 Implementation Details:**
- All spawn operations logged
- Structured JSON format
- Timestamps in RFC3339 format
- User identity captured (AWS IAM)

**Logged Events:**
- Instance launch/terminate
- Configuration changes
- Compliance validation
- Schedule creation/deletion
- Alert triggers
- Infrastructure access

**Code Reference:**
```go
// pkg/audit/logger.go
type AuditEvent struct {
    Timestamp   time.Time         `json:"timestamp"`
    EventType   string            `json:"event_type"`
    Actor       string            `json:"actor"`
    Action      string            `json:"action"`
    Resource    string            `json:"resource"`
    Status      string            `json:"status"`
    Details     map[string]string `json:"details,omitempty"`
}
```

### Identification and Authentication (IA)

| Control | Requirement | spawn Implementation | Evidence Location |
|---------|-------------|---------------------|-------------------|
| **IA-02** | Identification and Authentication | ✅ AWS IAM authentication | AWS SDK |
| **IA-05** | Authenticator Management | ✅ KMS secrets encryption | `pkg/security/secrets.go` |

**IA-02 Implementation Details:**
- AWS IAM credentials required for all spawn operations
- SigV4 authentication on all API calls
- MFA support (configured via AWS IAM)
- Session tokens for temporary credentials

**IA-05 Implementation Details:**
- SSH keys encrypted with KMS
- API tokens encrypted with KMS
- Secrets never logged in plaintext
- Automatic key rotation support

**Code Reference:**
```go
// pkg/security/secrets.go
func EncryptSecret(plaintext string) (string, error) {
    result, err := kmsClient.Encrypt(ctx, &kms.EncryptInput{
        KeyId:     aws.String("alias/spawn-secrets"),
        Plaintext: []byte(plaintext),
    })
    return base64.StdEncoding.EncodeToString(result.CiphertextBlob), err
}
```

### System and Communications Protection (SC)

| Control | Requirement | spawn Implementation | Evidence Location |
|---------|-------------|---------------------|-------------------|
| **SC-07** | Boundary Protection | ⚠️ Security group configuration | User-provided, validated |
| **SC-08** | Transmission Confidentiality | ✅ TLS 1.2+ for all API calls | AWS SDK (automatic) |
| **SC-12** | Cryptographic Key Establishment | ✅ KMS integration | `pkg/security/kms.go` |
| **SC-13** | Cryptographic Protection | ✅ FIPS-validated crypto | AWS SDK (automatic) |
| **SC-28** | Protection at Rest | ✅ EBS encryption enforcement | `pkg/compliance/nist80171.go:26-36` |

**SC-07 Implementation Details:**
- User must provide security group IDs
- spawn validates security groups exist
- Default security group used if subnet specified
- Compliance mode requires explicit configuration

**SC-08 Implementation Details:**
- AWS SDK uses TLS 1.2+ exclusively
- No plaintext API communication
- Certificate validation enforced
- FIPS 140-2 validated TLS implementation

**SC-12 Implementation Details:**
- KMS used for all key management
- Customer-managed keys supported
- Automatic key rotation available
- Key access logged to CloudTrail

**SC-13 Implementation Details:**
- AWS SDK uses FIPS 140-2 validated crypto modules
- AES-256 encryption for data at rest
- TLS 1.2+ for data in transit
- No deprecated algorithms used

**SC-28 Implementation Details:**
- EBS encryption enforced for all volumes
- AWS-managed or customer-managed KMS keys
- Encryption enabled before instance launch
- Cannot be disabled in compliance mode

**Code Reference:**
```go
// pkg/compliance/nist80171.go:26-36
Validator: func(cfg *aws.LaunchConfig) error {
    if !cfg.EBSEncrypted {
        return errors.New("EBS encryption required for NIST 800-171 (SC-28)")
    }
    return nil
},
Enforcer: func(cfg *aws.LaunchConfig) {
    cfg.EBSEncrypted = true
},
```

## NIST 800-53 Rev 5 Control Matrix

### Low Baseline (All NIST 800-171 + Enhanced Controls)

| Control | Requirement | Implementation | Baseline |
|---------|-------------|----------------|----------|
| All 800-171 controls | See above | ✅ Implemented | Low |

### Moderate Baseline (Low + Additional Controls)

| Control | Requirement | Implementation | Evidence Location |
|---------|-------------|----------------|-------------------|
| **SC-07(4)** | Private Subnets | ⚠️ User-configured private subnets | VPC configuration |
| **SC-28(1)** | Customer-Managed Keys | ⚠️ Recommended (not required) | `--ebs-kms-key-id` flag |
| **SI-02** | Flaw Remediation | 📋 Customer AMI patching | Customer process |

**SC-07(4) Implementation Details:**
- Moderate baseline recommends private subnets
- Public IPs should not be assigned
- User must configure VPC with private subnets
- spawn validates but doesn't enforce (Moderate)

**SC-28(1) Implementation Details:**
- Customer KMS keys recommended for Moderate
- Required for High baseline
- Key ID passed via `--ebs-kms-key-id` flag
- Key rotation configured by customer

**SI-02 Implementation Details:**
- Customer responsible for AMI selection
- Customer responsible for patching
- spawn does not manage OS updates
- Recommend using Amazon Linux 2 with automatic updates

### High Baseline (Moderate + Stringent Controls)

| Control | Requirement | Implementation | Evidence Location |
|---------|-------------|----------------|-------------------|
| **SC-28(1)** | Customer-Managed Keys | ✅ Required and enforced | `pkg/compliance/nist80053.go:146-158` |
| **SC-07(5)** | Deny by Default | ⚠️ Explicit security groups required | User-configured |
| **SC-07(12)** | VPC Endpoints | 📋 Customer VPC configuration | VPC endpoints |
| **CP-09** | System Backup | 📋 AWS Backup configuration | Customer process |
| **CP-10** | System Recovery | ⚠️ Multi-AZ recommended | `--multi-az` flag |

**SC-28(1) High Baseline Implementation:**
```go
// pkg/compliance/nist80053.go:146-158
Validator: func(cfg *aws.LaunchConfig) error {
    if cfg.EBSKMSKeyID == "" {
        return errors.New("customer-managed KMS key required for High baseline (SC-28(1))")
    }
    // Verify KMS key ID format
    if !strings.HasPrefix(cfg.EBSKMSKeyID, "arn:aws:kms:") &&
        !strings.HasPrefix(cfg.EBSKMSKeyID, "alias/") &&
        len(cfg.EBSKMSKeyID) != 36 {
        return errors.New("invalid KMS key ID format (SC-28(1))")
    }
    return nil
},
```

**SC-07(5) Implementation Details:**
- Security groups must be explicitly provided
- Default security group not allowed
- All rules should be deny-by-default with explicit allows
- spawn validates security group IDs provided

**SC-07(12) Implementation Details:**
- VPC endpoints for S3, DynamoDB, EC2 APIs
- Customer must configure in VPC
- Prevents traffic from leaving AWS network
- Required for High baseline compliance

**CP-09 Implementation Details:**
- AWS Backup service recommended
- EBS snapshots for volumes
- DynamoDB backups for infrastructure tables
- Customer defines retention policies

**CP-10 Implementation Details:**
- Multi-AZ deployment for resilience
- `--multi-az` flag launches across zones
- Load balancing across AZs
- Automatic failover in case of AZ failure

## FedRAMP Control Additions

FedRAMP adds organizational requirements beyond NIST 800-53:

| Requirement | spawn Role | Customer Role |
|-------------|------------|---------------|
| 3PAO Assessment | 📋 Provide technical evidence | Contract 3PAO, manage assessment |
| System Security Plan (SSP) | 📋 Provide control implementation details | Write and maintain SSP |
| Continuous Monitoring | ✅ Technical monitoring (CloudWatch) | 📋 Process and reporting |
| Incident Response | ✅ Alert generation | 📋 Incident response procedures |
| Vulnerability Scanning | 📋 Customer responsibility | Scan instances, remediate findings |
| Penetration Testing | 📋 Customer responsibility | Annual testing, report findings |

## Control Implementation Summary

### spawn Implements (Technical Controls)
- EBS encryption (SC-28, SC-28(1))
- IMDSv2 enforcement (AC-17)
- IAM least privilege (AC-06)
- Audit logging (AU-02, AU-03, AU-12)
- TLS encryption (SC-08, SC-13)
- KMS key management (IA-05, SC-12)
- Security group validation (SC-07)

### Customer Implements (Organizational + Configuration)
- Security policies and procedures
- Personnel security (background checks, training)
- Physical security
- Incident response procedures
- Configuration management
- Risk assessments
- System Security Plan (SSP)
- Third-party assessments (3PAO for FedRAMP)
- Vulnerability management
- Patch management
- VPC configuration (private subnets, VPC endpoints)
- Security group rules (deny-by-default)
- Backup configuration

## Validation Evidence

### Automated Validation

```bash
# Generate compliance report
spawn validate --nist-800-171 --output json > evidence/nist-800-171-$(date +%Y%m%d).json

# Generate baseline report
spawn validate --nist-800-53=high --output json > evidence/nist-800-53-high-$(date +%Y%m%d).json

# Infrastructure validation
spawn validate --infrastructure --output json > evidence/infrastructure-$(date +%Y%m%d).json
```

### Manual Verification

1. **EBS Encryption:**
   ```bash
   aws ec2 describe-volumes --volume-ids vol-xxx --query 'Volumes[0].Encrypted'
   ```

2. **IMDSv2 Enforcement:**
   ```bash
   aws ec2 describe-instances --instance-ids i-xxx --query 'Reservations[0].Instances[0].MetadataOptions'
   ```

3. **Security Groups:**
   ```bash
   aws ec2 describe-security-groups --group-ids sg-xxx
   ```

4. **Audit Logs:**
   ```bash
   aws logs filter-log-events --log-group-name /spawn/audit --start-time $(date -d '24 hours ago' +%s000)
   ```

## Audit Support

For compliance audits, provide:

1. **This control matrix document**
2. **Source code references** (GitHub repository)
3. **Automated validation reports** (JSON output)
4. **CloudWatch Logs** (audit trail)
5. **CloudTrail logs** (AWS API calls)
6. **Configuration files** (`~/.spawn/config.yaml`)
7. **Test results** (`go test ./pkg/compliance/... -cover`)

## Version History

| Version | Date | Changes |
|---------|------|---------|
| 1.0 | 2026-01-27 | Initial control matrix (v0.14.0) |

## Additional Resources

- [NIST 800-171 Quickstart](nist-800-171-quickstart.md)
- [NIST 800-53 Baselines Guide](nist-800-53-baselines.md)
- [Self-Hosted Infrastructure Guide](../how-to/self-hosted-infrastructure.md)
- [Audit Evidence Guide](audit-evidence.md)
