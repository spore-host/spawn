# Security Policy

## Overview

spawn takes security seriously. This document outlines security features, best practices, vulnerability disclosure policy, and informational guidance for the spawn platform.

**Last Updated:** 2026-01-27
**Current Version:** v0.13.0

---

## ⚠️ Important Disclaimers

### No Compliance Certifications

**spawn and spore.host make NO claims, representations, or warranties regarding compliance with any regulatory framework, security standard, or certification including but not limited to:**

- HIPAA (Health Insurance Portability and Accountability Act)
- PCI DSS (Payment Card Industry Data Security Standard)
- SOC 2 (Service Organization Control 2)
- NIST 800-171 (Controlled Unclassified Information)
- NIST 800-53 / FedRAMP (Federal Risk and Authorization Management Program)
- GDPR (General Data Protection Regulation)
- ISO 27001
- Any other compliance framework or certification

**The information in this document is provided for educational and informational purposes only. It is NOT a compliance certification, audit, attestation, or guarantee.**

### User Responsibility

**YOU are solely responsible for:**
- Assessing whether spawn meets your security and compliance requirements
- Implementing appropriate security controls for your use case
- Conducting security audits and compliance assessments
- Obtaining any necessary certifications or attestations
- Consulting with legal counsel and compliance officers
- Protecting your AWS credentials, SSH keys, and data
- Securing your EC2 instances and applications

### No Warranty

spawn is provided "AS IS" without warranty of any kind, express or implied, including but not limited to warranties of merchantability, fitness for a particular purpose, or non-infringement. Use at your own risk.

---

## Table of Contents

- [Reporting Security Issues](#reporting-security-issues)
- [Security Features](#security-features)
- [Security Model Overview](#security-model-overview)
- [IAM Permissions](#iam-permissions)
- [DNS Security Model](#dns-security-model)
- [Instance Identity Validation](#instance-identity-validation)
- [DNSSEC](#dnssec)
- [Cross-Account Security](#cross-account-security)
- [Data Protection](#data-protection)
- [Secrets Management](#secrets-management)
- [Network Security](#network-security)
- [Input Validation](#input-validation)
- [Audit and Logging](#audit-and-logging)
- [Security Best Practices](#security-best-practices)
- [Threat Model](#threat-model)
- [Compliance](#compliance)
- [Security Updates](#security-updates)

## Reporting Security Issues

### DO NOT create public GitHub issues for security vulnerabilities.

**Instead, please email:**
- **Security Contact:** scott@spore-host.dev
- **PGP Key:** Available at https://spore-host.dev/pgp-key.txt
- **Subject Line:** `[SECURITY] spawn vulnerability report`

### What to Include

Please provide:
1. **Description** of the vulnerability
2. **Steps to reproduce** the issue
3. **Impact assessment** (what an attacker could do)
4. **Affected versions** (if known)
5. **Suggested fix** (if you have one)
6. **Your contact information** for follow-up

### Response Timeline

- **24 hours:** Initial acknowledgment
- **7 days:** Vulnerability assessment and triage
- **30 days:** Fix development and testing
- **90 days:** Public disclosure (coordinated with reporter)

### Disclosure Policy

We follow **coordinated disclosure**:
1. Reporter notifies us privately
2. We confirm and assess the vulnerability
3. We develop and test a fix
4. We release a patched version
5. We publicly disclose the vulnerability (after fix is available)
6. We credit the reporter (if desired)

---

## Security Features

### Authentication & Authorization

**AWS IAM-Based Authentication:**
- spawn uses AWS IAM credentials exclusively (no spawn-specific credentials)
- Supports all AWS credential sources: environment variables, config files, IAM roles
- Instance authentication via temporary IAM instance profile credentials
- Automatic credential rotation (instance credentials rotate every 6 hours)

**IAM Policy Templates:**
- Pre-defined least-privilege policy templates (`s3:ReadOnly`, `dynamodb:ReadOnly`, etc.)
- Custom policy file support for specific requirements
- Scoped resource policies (region and user-specific)
- Automatic IAM role creation and reuse

### Encryption

**Data at Rest:**
- EBS volume encryption with AWS-managed or customer-managed KMS keys
- S3 server-side encryption enabled by default
- DynamoDB encryption at rest enabled by default
- Encrypted secrets storage using AWS KMS

**Data in Transit:**
- All AWS API calls over HTTPS/TLS 1.2+
- SSH encrypted connections for instance access
- Certificate validation enabled and enforced

### Network Security

**Security Groups:**
- Automatic security group creation with configurable rules
- Support for restrictive CIDR blocks
- MPI-specific security groups with cluster-only communication
- VPC and subnet selection for network isolation

**Private Networking:**
- Support for private subnets (no public IP)
- AWS Systems Manager Session Manager support (SSH alternative)
- VPC endpoint support for private AWS service access

### Input Validation

**Command Injection Prevention:**
- All user inputs sanitized and validated before shell execution
- Shell escape functions for command arguments (`pkg/security/shell.go`)
- Username validation (POSIX-compliant format)
- Base64 validation for encoded data

**Path Traversal Prevention:**
- File path validation for user data scripts (`pkg/security/path.go`)
- Forbidden path checking (no access to `/etc`, `/sys`, `/proc`, `/root`)
- Symlink traversal prevention

### Audit Logging

**Structured Audit Logs:**
- All privileged operations logged with correlation IDs (`pkg/audit/logger.go`)
- User identity tracking for all operations
- Instance lifecycle events (launch, terminate, extend)
- IAM policy creation and modification logs
- Alert configuration changes

---

## Security Model Overview

Spawn uses a defense-in-depth approach with multiple security layers:

1. **IAM-based authentication** - AWS credentials validate user identity
2. **Instance tagging** - `spawn:managed=true` tag identifies managed instances
3. **Cryptographic validation** - AWS-signed instance identity documents prove authenticity
4. **Least privilege** - Minimal IAM permissions required for operation
5. **Audit trail** - CloudWatch logs track all operations

## IAM Permissions

Spawn requires specific IAM permissions to operate. See [IAM_PERMISSIONS.md](IAM_PERMISSIONS.md) for detailed permission requirements.

### Principle of Least Privilege

Spawn follows the principle of least privilege:

- **User accounts** - Only require EC2, SSM, and optional DNS permissions
- **No admin access** - spawn never requires AdministratorAccess
- **Read-only where possible** - Many operations only need describe/list permissions
- **Scoped permissions** - IAM policies can be scoped to specific regions/resources

### SSH Key Security

Spawn manages SSH keys securely:

- Keys stored in `~/.ssh/spawn-*.pem` with 0600 permissions
- Never transmitted over network (except via SSH agent)
- Unique key per instance
- Automatically cleaned up on instance termination

## DNS Security Model

Spawn provides automatic DNS registration for instances via the spore.host domain (or custom domains for institutions).

### Architecture

The DNS system uses a **serverless API Gateway + Lambda** architecture:

```
┌─────────────────┐
│  EC2 Instance   │
│ (Any AWS Acct)  │
└────────┬────────┘
         │ 1. Get instance identity from IMDS
         │ 2. Call DNS API
         ▼
┌─────────────────────────────────────┐
│   API Gateway (Public Endpoint)     │
│  https://....amazonaws.com/prod     │
└────────┬────────────────────────────┘
         │ 3. Invoke Lambda
         ▼
┌─────────────────────────────────────┐
│   Lambda Function (Go)              │
│   - Validate instance identity      │
│   - Check spawn:managed tag         │
│   - Verify IP address               │
└────────┬────────────────────────────┘
         │ 4. Update DNS
         ▼
┌─────────────────────────────────────┐
│   Route53 Hosted Zone               │
│   spore.host (Z048907324UNXKEK9KX93)│
└─────────────────────────────────────┘
```

### Security Guarantees

✅ **No shared secrets** - Instance identity documents are cryptographically signed by AWS and cannot be forged

✅ **Per-instance validation** - Each DNS update request is validated against live AWS instance metadata

✅ **IP verification** - DNS records only updated if IP address matches the instance's actual public IP (for same-account instances)

✅ **Tag enforcement** - Only instances with `spawn:managed=true` tag can update DNS (for same-account instances)

✅ **No IAM roles required** - User accounts don't need special Route53 permissions or cross-account IAM trust relationships

✅ **Audit trail** - All DNS update requests logged in CloudWatch Logs with full request details

✅ **Rate limiting** - API Gateway provides DDoS protection and rate limiting

✅ **DNSSEC enabled** - Cryptographic signatures prevent DNS hijacking and cache poisoning

### Instance Identity Validation

The security model relies on AWS Instance Identity Documents:

1. **What it is**: A JSON document containing instance metadata (instance ID, region, account ID, IP address)

2. **Cryptographic signature**: AWS signs the document with a private key that only AWS possesses

3. **Verification**: The Lambda function validates the signature using AWS's public key

4. **Cannot be forged**: Without AWS's private key, attackers cannot create valid instance identity documents

5. **Retrieved from IMDS**: Only accessible from within the EC2 instance via the Instance Metadata Service

### Validation Flow

When an instance requests DNS registration:

```go
// 1. Instance retrieves identity from IMDSv2 (token-based)
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")

IDENTITY_DOC=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/dynamic/instance-identity/document | base64 -w0)

IDENTITY_SIG=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/dynamic/instance-identity/signature)

// 2. Lambda validates the request
func validateInstance(ctx, instanceID, region, ipAddress, action) error {
    // Try to describe instance (works for same-account)
    output, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
        InstanceIds: []string{instanceID},
    })

    if err != nil {
        // Cross-account: Can't describe instance in another account
        // Rely on instance identity signature validation
        // This is secure because signature cannot be forged
        return nil
    }

    // Same-account: Perform full validation
    // - Check spawn:managed tag
    // - Verify IP address matches
    // - Check instance state (running/stopped)

    return nil
}
```

### API Request Format

```bash
POST https://f4gm19tl70.execute-api.us-east-1.amazonaws.com/prod/update-dns
Content-Type: application/json

{
  "instance_identity_document": "<base64-encoded-document>",
  "instance_identity_signature": "<base64-encoded-signature>",
  "record_name": "my-instance",
  "ip_address": "1.2.3.4",
  "action": "UPSERT"  // or "DELETE"
}
```

### Lambda IAM Permissions

The Lambda function has minimal permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "route53:ChangeResourceRecordSets",
        "route53:ListResourceRecordSets"
      ],
      "Resource": "arn:aws:route53:::hostedzone/Z048907324UNXKEK9KX93"
    },
    {
      "Effect": "Allow",
      "Action": "ec2:DescribeInstances",
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogGroup",
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "arn:aws:logs:*:*:*"
    }
  ]
}
```

## Instance Identity Validation

### IMDSv2 (Recommended)

Spawn uses IMDSv2 (token-based) for retrieving instance identity:

**Benefits:**
- Prevents SSRF attacks
- Requires token authentication
- Session-oriented

**How it works:**
```bash
# 1. Get session token
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")

# 2. Use token for all subsequent requests
INSTANCE_ID=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/instance-id)
```

### Instance Identity Document Structure

```json
{
  "instanceId": "i-1234567890abcdef0",
  "region": "us-east-1",
  "accountId": "123456789012",
  "architecture": "x86_64",
  "imageId": "ami-0abcdef1234567890",
  "instanceType": "t3.micro",
  "privateIp": "10.0.1.5",
  "availabilityZone": "us-east-1a",
  "version": "2017-09-30",
  "pendingTime": "2023-01-01T12:00:00Z"
}
```

The signature ensures this document is authentic and was issued by AWS.

## DNSSEC

Spawn's DNS infrastructure uses DNSSEC for additional security.

### What is DNSSEC?

DNSSEC (DNS Security Extensions) adds cryptographic signatures to DNS records to prevent:

- **DNS hijacking** - Attackers redirecting your DNS to malicious servers
- **Cache poisoning** - Malicious DNS entries injected into caches
- **Man-in-the-middle attacks** - Intercepting DNS queries to redirect traffic

### Implementation

**KMS Key**: ECC_NIST_P256 key for signing DNS records
- Key ID: `b638147e-f2c0-48bd-a3a6-5f1b7d4773d0`
- Algorithm: ECDSAP256SHA256 (Algorithm 13)
- Managed by AWS KMS with automatic rotation

**Key Signing Key (KSK)**: spore-host-ksk
- Key Tag: 12735
- DS Record: `12735 13 2 0179EFB5FA92E41D46256E7C1D8628B9DD7C0529E85E400F9B48213685BBA5E4`

**Zone Signing Key (ZSK)**: Automatically rotated by Route53

### Verification

Check DNSSEC status:

```bash
# Check for DNSSEC signatures
dig +dnssec spore.host SOA

# Validate DNSSEC chain
delv spore.host

# Online validators
https://dnssec-debugger.verisignlabs.com/spore.host
https://dnsviz.net/d/spore.host/dnssec/
```

### Security Benefits

When you connect to `my-instance.spore.host`:

1. DNS query sent to resolver
2. Resolver gets DNS record + DNSSEC signature
3. Resolver validates signature using public key
4. Only if signature is valid, IP address is returned
5. You connect to the authentic instance IP

This prevents attackers from redirecting you to malicious instances.

## Cross-Account Security

Spawn is designed to work securely across AWS accounts without requiring IAM trust relationships.

### Same-Account Instances

When spawn and the instance are in the same AWS account:

✅ **Full validation**:
- Verify `spawn:managed=true` tag
- Verify IP address matches instance public IP
- Verify instance state (running/stopped)
- Validate instance identity signature

### Cross-Account Instances

When spawn and the instance are in different AWS accounts:

✅ **Signature-based validation**:
- Validate instance identity signature (cryptographic proof)
- Cannot check tags or IP (no DescribeInstances permission cross-account)
- Relies on AWS cryptographic signature as primary security

**Why this is secure**:
- Instance identity signatures cannot be forged without AWS's private key
- Only legitimate EC2 instances can retrieve valid identity documents
- Signature proves the instance exists and is running in the claimed account

### No IAM Trust Required

Traditional cross-account access requires:
```json
// ❌ Traditional approach - doesn't scale for open source
{
  "Effect": "Allow",
  "Principal": {
    "AWS": [
      "arn:aws:iam::111111111111:root",
      "arn:aws:iam::222222222222:root",
      // ... maintain allowlist of every user account
    ]
  }
}
```

Spawn's approach:
```
✅ No trust relationship required
✅ No allowlist to maintain
✅ Works for any AWS account
✅ Cryptographic validation via instance identity
```

## Audit and Logging

### CloudWatch Logs

All DNS operations are logged to CloudWatch Logs:

- Lambda function: `/aws/lambda/spawn-dns-updater`
- Log group retention: 30 days (configurable)
- Logs include:
  - Timestamp
  - Instance ID
  - AWS account ID
  - Region
  - Requested DNS record
  - IP address
  - Action (UPSERT/DELETE)
  - Validation results
  - Errors (if any)

### Example Log Entry

```json
{
  "timestamp": "2025-12-21T18:00:00Z",
  "requestId": "abc123-def456",
  "instanceId": "i-1234567890abcdef0",
  "accountId": "123456789012",
  "region": "us-east-1",
  "recordName": "my-instance",
  "fqdn": "my-instance.spore.host",
  "ipAddress": "54.164.27.106",
  "action": "UPSERT",
  "validation": "success",
  "changeId": "/change/C123456789",
  "message": "DNS record updated successfully"
}
```

### Monitoring

Recommended CloudWatch alarms:

- **DNS API errors** - Alert on 5xx responses
- **Validation failures** - Alert on failed instance validation
- **Rate limiting** - Alert on API Gateway throttling
- **Lambda errors** - Alert on Lambda function failures

## Data Protection

### Encryption at Rest

**EBS Volumes:**
```bash
# Enable encryption with AWS-managed key
spawn launch --encrypt-volumes

# Use customer-managed KMS key
spawn launch --encrypt-volumes --kms-key-id arn:aws:kms:us-east-1:123456789012:key/xxx
```

**S3 Buckets:**
- Server-side encryption (SSE-S3) enabled by default
- Support for SSE-KMS with customer-managed keys
- Client-side encryption available for sensitive data

**DynamoDB:**
- Encryption at rest enabled by default on all spawn tables
- Uses AWS-managed KMS keys

### Encryption in Transit

**AWS API Calls:**
- All API calls use HTTPS/TLS 1.2+
- Certificate validation enabled and enforced
- No option to disable TLS verification

**SSH Connections:**
- Encrypted by default (SSH protocol)
- Support for ED25519 and RSA-4096 keys
- Key-based authentication only (no passwords)

---

## Secrets Management

### AWS Secrets Manager Integration

**Store secrets securely:**
```bash
# Store API key
aws secretsmanager create-secret \
  --name myapp/api-key \
  --secret-string "sk-abc123xyz789"

# Launch with Secrets Manager access
spawn launch \
  --iam-policy secretsmanager:ReadOnly \
  --user-data "
    API_KEY=\$(aws secretsmanager get-secret-value \
      --secret-id myapp/api-key \
      --query SecretString \
      --output text)
    export API_KEY
    python app.py
  "
```

### SSM Parameter Store

**For less sensitive configuration:**
```bash
# Store encrypted parameter
aws ssm put-parameter \
  --name /myapp/config \
  --value "config-value" \
  --type SecureString

# Retrieve in user data
spawn launch \
  --iam-policy ssm:ReadOnly \
  --user-data "
    CONFIG=\$(aws ssm get-parameter \
      --name /myapp/config \
      --with-decryption \
      --query Parameter.Value \
      --output text)
  "
```

### Webhook URL Encryption

Webhook URLs for alerts are encrypted using KMS:
- Stored encrypted in DynamoDB (`pkg/security/secrets.go`)
- Decrypted only when needed by alert Lambda
- Masked in CLI output and logs

### Anti-Patterns

**❌ Never hardcode secrets:**
```bash
# BAD: Secret in user data
spawn launch --user-data "
  export API_KEY=sk-abc123xyz789
  python app.py
"
```

**✅ Use Secrets Manager:**
```bash
# GOOD: Retrieve from Secrets Manager
spawn launch --iam-policy secretsmanager:ReadOnly --user-data "
  API_KEY=\$(aws secretsmanager get-secret-value \
    --secret-id myapp/api-key --query SecretString --output text)
  python app.py
"
```

---

## Network Security

### Security Group Best Practices

**Default (Development):**
```bash
# Allows SSH from anywhere (0.0.0.0/0)
spawn launch --instance-type t3.micro
```

**Restrictive (Production):**
```bash
# Create restrictive security group
MY_IP=$(curl -s ifconfig.me)
aws ec2 create-security-group \
  --group-name spawn-secure \
  --description "Secure spawn instances"

aws ec2 authorize-security-group-ingress \
  --group-id sg-xxx \
  --protocol tcp \
  --port 22 \
  --cidr $MY_IP/32

# Launch with restricted access
spawn launch --instance-type t3.micro --security-groups sg-xxx
```

### Private Subnet Deployment

**For sensitive workloads:**
```bash
# Launch in private subnet (no public IP)
spawn launch \
  --instance-type t3.micro \
  --subnet subnet-private-xxx \
  --no-public-ip \
  --security-groups sg-private

# Connect via Session Manager (no SSH required)
spawn connect i-xxx --ssm
```

### IMDSv2 (Instance Metadata Service v2)

**Prevent SSRF attacks:**
```bash
spawn launch --instance-type t3.micro \
  --metadata-options "HttpTokens=required"
```

**Why IMDSv2:**
- Token-based authentication
- Prevents SSRF credential theft
- Session-oriented (tokens expire)

---

## Input Validation

spawn implements comprehensive input validation to prevent injection attacks.

### Command Injection Prevention

**Shell Escaping (`pkg/security/shell.go`):**
```go
// All user inputs are escaped before shell execution
sporedCmd := fmt.Sprintf("sudo /usr/local/bin/spored config set %s %s",
    security.ShellEscape(key),
    security.ShellEscape(value))
```

**Username Validation:**
```go
// POSIX-compliant username format enforced
func ValidateUsername(username string) error {
    matched, _ := regexp.MatchString(`^[a-z][a-z0-9_-]{0,31}$`, username)
    if !matched {
        return errors.New("invalid username format")
    }
    return nil
}
```

### Path Traversal Prevention

**File Path Validation (`pkg/security/path.go`):**
```go
// Prevents access to system paths
func ValidatePathForReading(path string) error {
    cleaned := filepath.Clean(path)

    // Block path traversal
    if strings.Contains(cleaned, "..") {
        return errors.New("path traversal not allowed")
    }

    // Block system paths
    forbidden := []string{"/etc/", "/sys/", "/proc/", "/root/"}
    for _, prefix := range forbidden {
        if strings.HasPrefix(abs, prefix) {
            return errors.New("access to system paths not allowed")
        }
    }

    return nil
}
```

---

## Security Best Practices

### For Users

#### 1. Use Restrictive Security Groups

**Don't:**
```bash
spawn launch --instance-type t3.micro  # Allows SSH from 0.0.0.0/0
```

**Do:**
```bash
MY_IP=$(curl -s ifconfig.me)
spawn launch --instance-type t3.micro \
  --security-groups sg-restrictive-$MY_IP
```

#### 2. Enable EBS Encryption

**Don't:**
```bash
spawn launch --instance-type t3.micro
```

**Do:**
```bash
spawn launch --instance-type t3.micro --encrypt-volumes
```

#### 3. Use Private Subnets for Sensitive Workloads

**Don't:**
```bash
spawn launch --instance-type t3.micro  # Public subnet
```

**Do:**
```bash
spawn launch --instance-type t3.micro \
  --subnet subnet-private-xxx \
  --no-public-ip \
  --security-groups sg-private
```

#### 4. Use AWS Secrets Manager for Secrets

**Don't:**
```bash
spawn launch --user-data "export API_KEY=sk-abc123"
```

**Do:**
```bash
aws secretsmanager create-secret --name myapp/api-key --secret-string "sk-abc123"
spawn launch --iam-policy secretsmanager:ReadOnly --user-data "
  API_KEY=\$(aws secretsmanager get-secret-value --secret-id myapp/api-key --query SecretString --output text)
"
```

#### 5. Enable IMDSv2

**Do:**
```bash
spawn launch --instance-type t3.micro --metadata-options "HttpTokens=required"
```

#### 6. Use Least-Privilege IAM Policies

**Don't:**
```bash
spawn launch --iam-policy-file full-access.json  # Too broad
```

**Do:**
```bash
spawn launch --iam-policy s3:ReadOnly  # Scoped template
```

#### 7. Tag Instances with Owner

```bash
spawn launch --instance-type t3.micro \
  --tags owner=alice,project=ml,cost-center=research
```

#### 8. Monitor with Alerts

```bash
spawn alerts create my-sweep \
  --on-cost-threshold 100 \
  --email alerts@company.com
```

#### 9. Use Strong SSH Keys

```bash
# ED25519 (recommended)
ssh-keygen -t ed25519 -C "spawn-key" -f ~/.ssh/spawn-ed25519

# Or RSA-4096
ssh-keygen -t rsa -b 4096 -C "spawn-key" -f ~/.ssh/spawn-rsa
```

#### 10. Review Audit Logs Regularly

```bash
# CloudTrail events
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=ResourceType,AttributeValue=AWS::EC2::Instance

# DynamoDB instance history
aws dynamodb query \
  --table-name spawn-instances \
  --key-condition-expression "user_id = :uid" \
  --expression-attribute-values '{":uid":{"S":"alice"}}'
```

#### 11. Use IMDSv2

**Enabled by default in spawn**

#### 12. Rotate SSH Keys

spawn generates unique keys per instance

#### 13. Use Short TTLs

```bash
spawn launch --ttl 2h  # For development instances
```

#### 14. Clean Up Instances

```bash
spawn stop <instance-id>  # Terminate when done
```

#### 15. Review DNS Records

```bash
# Periodically audit *.spore.host records
dig @8.8.8.8 *.spore.host ANY
```

#### 16. Enable MFA

Use MFA on your AWS account

#### 17. Use Least Privilege

Only grant necessary IAM permissions

### For Institutions (Custom DNS)

1. **Deploy in isolated account** - Separate account for DNS infrastructure
2. **Enable CloudTrail** - Audit all Route53 API calls
3. **Set up alarms** - Monitor DNS API for anomalies
4. **Review Lambda logs** - Regularly audit DNS update requests
5. **Enable DNSSEC** - Prevent DNS hijacking
6. **Use KMS encryption** - Encrypt CloudWatch logs
7. **Implement backup** - Export Route53 zone regularly

### For Contributors

1. **Never commit secrets** - Use AWS credentials from environment
2. **Review IAM policies** - Ensure least privilege
3. **Test cross-account** - Validate cross-account scenarios
4. **Validate input** - Always validate user input in Lambda
5. **Use prepared statements** - Prevent injection attacks
6. **Enable security scanning** - Use Dependabot, CodeQL
7. **Follow secure coding** - OWASP guidelines for Go

---

## Threat Model

### Assets to Protect

**Credentials:**
- AWS access keys and secret keys
- SSH private keys
- Application API keys and tokens
- Slack/webhook URLs

**Data:**
- Source code
- Training data and model weights
- Computation results
- Configuration files

**Infrastructure:**
- EC2 instances (compute costs)
- S3 buckets (data storage)
- DynamoDB tables (metadata)
- Lambda functions (orchestration)

### Threat Actors

**External Attackers:**
- Attempting to compromise AWS accounts
- Scanning for exposed services and ports
- Exploiting vulnerable instances or applications

**Insider Threats:**
- Accidental misconfiguration
- Over-privileged IAM policies
- Leaked credentials in code repositories

**Supply Chain:**
- Compromised dependencies (Go modules, Docker images)
- Malicious AMIs
- Tampered binaries

### Attack Vectors

**Credential Compromise:**
- Leaked AWS keys in Git repositories
- SSH keys with weak passphrases
- Credentials in environment variables logged
- Metadata service exploitation (SSRF)

**Network Attacks:**
- SSH brute force (0.0.0.0/0 security groups)
- Man-in-the-middle (unencrypted traffic)
- Lateral movement within VPC

**Injection Attacks:**
- Command injection in user data scripts
- Path traversal in file operations

**Resource Abuse:**
- Cryptocurrency mining on forgotten instances
- Excessive AWS spending from unmonitored launches

### What spawn Protects Against

- ✅ Command injection (input sanitization)
- ✅ Path traversal (file path validation)
- ✅ Unauthorized DNS updates (instance identity validation)
- ✅ DNS hijacking (DNSSEC)
- ✅ Cache poisoning (DNSSEC)
- ✅ Cross-account abuse (tag enforcement + signature validation)
- ✅ Unauthorized instance access (SSH key management)
- ✅ Credential leaks (secrets encryption, log sanitization)

### What spawn Does NOT Protect Against

- ❌ Compromised AWS credentials (use MFA, rotate keys)
- ❌ Compromised EC2 instances (harden your instances)
- ❌ AWS account compromise (enable CloudTrail, GuardDuty)
- ❌ Physical access to infrastructure (AWS's responsibility)
- ❌ Application-level vulnerabilities (secure your code)

---

## Compliance

---

**⚠️ IMPORTANT COMPLIANCE DISCLAIMER ⚠️**

**spawn and spore.host make NO representations, warranties, or certifications regarding compliance with any regulatory framework, standard, or requirement, including but not limited to HIPAA, PCI DSS, SOC 2, NIST 800-171, NIST 800-53, FedRAMP, GDPR, or any other compliance framework.**

**Key Points:**
- spawn **actively works to align with** common compliance frameworks by implementing security controls and features
- spawn is an **open-source tool** that CAN BE CONFIGURED to support your compliance efforts
- **You are solely responsible** for your own compliance assessments and certifications
- **You must conduct your own** security audits, risk assessments, and compliance validations
- **No warranties** are provided regarding the suitability of spawn for any specific compliance requirement
- **The spore.host service** is provided as-is without any compliance certifications or guarantees
- **Consult your compliance officers** and legal counsel before using spawn for regulated workloads

**This section provides INFORMATIONAL GUIDANCE** on how spawn features are designed to align with common compliance controls. It is NOT a certification, attestation, or guarantee of compliance.

---

spawn is designed to support common compliance frameworks through security features and controls. The following information describes how spawn CAN BE CONFIGURED to support your compliance requirements. Users must perform their own compliance assessments:

### HIPAA (Health Insurance Portability and Accountability Act)

**⚠️ spawn makes no representations regarding HIPAA compliance. HIPAA compliance is a risk acceptance framework - there is no "HIPAA certification." However, spawn is designed to support HIPAA compliance efforts through security controls and features.**

spawn provides security features aligned with common HIPAA security and privacy requirements. If you are working to meet HIPAA requirements, typical elements of a HIPAA compliance program include:
1. Sign AWS Business Associate Agreement (BAA) with AWS (separate from spawn)
2. Use HIPAA-eligible AWS services (EC2, S3, DynamoDB, Lambda)
3. Encrypt all ePHI (Electronic Protected Health Information)
4. Enable audit logging (CloudTrail, access logs)
5. Implement access controls (IAM, MFA, role-based access)
6. Conduct risk assessments and document policies/procedures
7. Implement administrative, physical, and technical safeguards
8. Execute Business Associate Agreements with service providers

**Example spawn configuration to support HIPAA requirements** (user must conduct own compliance assessment):
```bash
spawn launch \
  --encrypt-volumes \
  --kms-key-id <hipaa-compliant-key> \
  --iam-policy <least-privilege-policy> \
  --no-public-ip \
  --subnet <private-subnet> \
  --tags compliance=hipaa,data-class=phi
```

### PCI DSS (Payment Card Industry Data Security Standard)

**⚠️ spawn has NOT been assessed for PCI DSS compliance and makes no representations regarding PCI DSS compliance. However, spawn is designed to support PCI DSS compliance efforts through security controls and features.**

spawn provides security features aligned with common PCI DSS requirements. If you are working to meet PCI DSS requirements (for cardholder data environment), typical requirements include:
1. Network segmentation (separate VPC/subnets for CDE)
2. Encryption (TLS 1.2+, AES-256 for data at rest and in transit)
3. Access controls (MFA, unique IDs, audit logs)
4. Vulnerability scanning (quarterly by ASV)
5. Penetration testing (annually)
6. Obtain PCI DSS attestation from Qualified Security Assessor (QSA)

**Example spawn configuration to support PCI DSS requirements** (user must conduct own compliance assessment):
```bash
spawn launch \
  --vpc vpc-pci \
  --subnet subnet-pci-private \
  --security-groups sg-pci-restrictive \
  --encrypt-volumes \
  --tags compliance=pci-dss,cardholder-data=yes
```

### SOC 2 Type II

**⚠️ spawn has NOT undergone a SOC 2 audit and makes no representations regarding SOC 2 compliance. However, spawn is designed to support SOC 2 compliance efforts through security controls and features.**

spawn provides security features aligned with common SOC 2 Trust Services Criteria. If you are working to obtain a SOC 2 Type II report, typical requirements include:
1. Security policies and procedures (documented)
2. Access control policies and implementation
3. Encryption (data at rest and in transit)
4. Audit logging (all administrative actions)
5. Change management processes
6. Incident response procedures
7. Vendor management program
8. Obtain SOC 2 Type II audit from qualified CPA firm

**spawn security features designed to support SOC 2 requirements** (user must obtain SOC 2 audit):
- IAM-based access control (available)
- Optional EBS/S3 encryption (user-configured)
- CloudTrail audit logs (user-configured)
- DynamoDB state tracking (built-in)
- Change management (user responsibility)

### NIST 800-171 Rev 3

**⚠️ spawn has NOT been assessed for NIST 800-171 compliance and makes no representations regarding NIST 800-171 compliance. However, spawn is designed to support NIST 800-171 compliance efforts through security controls and features.**

**Status:** Enhanced compliance mode features planned but NOT yet implemented (issue #64)

NIST 800-171 addresses Controlled Unclassified Information (CUI) protection requirements for federal contractors. spawn provides security features aligned with NIST 800-171 security requirements:
- Access control (AC) - IAM-based authentication, least privilege
- Audit and accountability (AU) - CloudWatch Logs, CloudTrail
- Configuration management (CM) - Infrastructure as code support
- Identification and authentication (IA) - IAM roles, MFA support
- System and communications protection (SC) - Encryption, network segmentation
- System and information integrity (SI) - Input validation, security monitoring

Organizations must conduct their own NIST 800-171 self-assessment or third-party assessment

### NIST 800-53 Rev 5 (FedRAMP)

**⚠️ spawn is NOT FedRAMP authorized and has NOT been assessed for NIST 800-53 compliance. spawn makes no representations regarding NIST 800-53 or FedRAMP compliance. However, spawn is designed to support NIST 800-53 compliance efforts through security controls and features.**

**Status:** Enhanced compliance mode features planned but NOT yet implemented (issue #65)

NIST 800-53 provides comprehensive security controls used by the Federal Risk and Authorization Management Program (FedRAMP). spawn provides security features aligned with NIST 800-53 control families:
- Access Control (AC) - IAM-based authentication and authorization
- Audit and Accountability (AU) - Comprehensive logging and monitoring
- Configuration Management (CM) - Infrastructure state tracking
- Identification and Authentication (IA) - Multi-factor authentication support
- System and Communications Protection (SC) - Encryption, network security
- System and Information Integrity (SI) - Input validation, integrity checking

FedRAMP authorization requires:
- Authorization Boundary documentation
- System Security Plan (SSP)
- Continuous monitoring
- Third-party assessment by 3PAO (FedRAMP Authorized Assessor)
- ATO (Authority to Operate) from sponsoring agency

Organizations must pursue their own FedRAMP authorization process

### HITRUST CSF (Common Security Framework)

**⚠️ spawn is NOT HITRUST certified and makes no representations regarding HITRUST certification. However, spawn is designed to support HITRUST CSF compliance efforts through security controls and features.**

HITRUST CSF is a certifiable framework widely adopted in healthcare that incorporates controls from HIPAA, NIST, ISO, and other standards. spawn provides security features aligned with HITRUST CSF requirements:

**HITRUST Control Categories supported:**
- **Access Control** - IAM-based authentication, role-based access, MFA support
- **Audit Logging & Monitoring** - CloudWatch Logs, CloudTrail, structured audit events
- **Data Protection & Privacy** - Encryption at rest and in transit, KMS integration
- **Risk Management** - Security features designed with risk-based approach
- **Incident Management** - Logging and monitoring capabilities for incident detection
- **Configuration Management** - Infrastructure state tracking, change control support
- **Network Protection** - Security groups, private subnets, network segmentation

**HITRUST Certification Process:**
HITRUST CSF has formal certification levels:
- **HITRUST i1 Implementation** - Self-assessment
- **HITRUST r2 Report** - Validated assessment (external assessor)
- **HITRUST Certified** - Full certification (comprehensive assessment)

Organizations must engage a HITRUST assessor and pursue their own HITRUST certification. spawn provides security controls that can support a HITRUST CSF implementation.

### General Security Checklist

**This is a general security checklist, NOT a compliance certification. You must conduct your own compliance assessments.**

- [ ] **Encryption:** EBS volumes encrypted with customer-managed KMS keys
- [ ] **Networking:** Private subnets, restrictive security groups
- [ ] **IAM:** Least-privilege policies, no long-term credentials
- [ ] **Audit:** CloudTrail enabled, logs retained 90+ days
- [ ] **Tagging:** Compliance tags on all resources
- [ ] **Secrets:** No credentials in user data, use Secrets Manager
- [ ] **Access:** MFA enabled, Systems Manager Session Manager
- [ ] **Monitoring:** CloudWatch alarms, centralized logging
- [ ] **Documentation:** Architecture diagrams, security policies
- [ ] **Testing:** Quarterly vulnerability scans, annual penetration tests

---

## Security Updates

### Notification Channels

Subscribe to security updates:
- **GitHub Watch:** "Releases only" or "All activity"
- **Security Advisories:** https://github.com/spore-host/spore-host/security/advisories

### Release Notes Format

Security fixes are clearly marked:
- 🔒 **SECURITY:** Critical security update
- ⚠️ **WARNING:** Breaking change for security reasons

**Example:**
```
## v0.13.1 - 2026-02-01

🔒 SECURITY FIXES:
- Fixed command injection vulnerability in config command (CVE-2026-XXXX)
- Updated AWS SDK to fix credential leak in error messages

IMPACT: High
AFFECTED VERSIONS: v0.1.0 - v0.13.0
UPGRADE URGENCY: Immediate
```

### Update Policy

- **Critical vulnerabilities:** Patched within 7 days
- **High vulnerabilities:** Patched within 30 days
- **Medium/Low vulnerabilities:** Included in next regular release

**Supported Versions:**
- **Latest minor version:** Full support (security + bug fixes)
- **Previous minor version:** Security fixes only (6 months)
- **Older versions:** No support (please upgrade)

---

## Security Considerations

### Known Limitations

1. **Cross-account validation** - Cannot verify tags or IP address for instances in other AWS accounts (relies on signature validation only)

2. **DNS propagation** - DNS changes take time to propagate (60s TTL by default)

3. **Rate limiting** - API Gateway has rate limits (10,000 requests/second default)

4. **Instance identity freshness** - Identity documents don't expire, but instance must be running to retrieve them

### Future Enhancements

- [ ] Implement signature verification in Lambda (currently relies on same-account validation)
- [ ] Add support for IPv6 DNS records (AAAA)
- [ ] Implement DNS record TTL customization per instance
- [ ] Add support for TXT records (for metadata)
- [ ] Implement rate limiting per AWS account
- [ ] Add support for custom SSL certificates

## Compliance Notes

**⚠️ NO COMPLIANCE CERTIFICATIONS:** spawn and spore.host are NOT certified, audited, or compliant with any regulatory framework. However, spawn is actively designed with compliance frameworks in mind.

**Security capabilities designed to support compliance efforts** (subject to your own assessment):

- **Audit Logging** - CloudWatch Logs, CloudTrail integration, structured audit events
- **Access Controls** - IAM-based authentication, least-privilege policy templates
- **Encryption** - EBS encryption, S3 server-side encryption, KMS integration
- **Input Validation** - Command injection prevention, path traversal protection
- **Secrets Management** - KMS-encrypted webhook storage, Secrets Manager integration
- **Network Security** - Private subnet support, security group management, IMDSv2
- **Data Handling** - No PII stored by spore.host (only instance IDs, IP addresses)
- **Regional Support** - Compatible with AWS GovCloud regions (user-deployed)

**⚠️ YOU are responsible for:**
- Conducting compliance assessments and audits
- Implementing all required security controls
- Obtaining certifications and attestations
- Meeting all regulatory requirements
- Consulting compliance and legal counsel

## Additional Resources

### spawn Documentation

- [Architecture Overview](docs/explanation/architecture.md)
- [Security Model Explanation](docs/explanation/security-model.md)
- [IAM Policies Reference](docs/reference/iam-policies.md)
- [How-To: Security & IAM](docs/how-to/security-iam.md)
- [CONTRIBUTING.md](CONTRIBUTING.md)

### AWS Security Resources

- [AWS Security Best Practices](https://aws.amazon.com/architecture/security-identity-compliance/)
- [AWS Instance Identity Documents](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instance-identity-documents.html)
- [IMDSv2 Documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html)
- [Route53 DNSSEC](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/dns-configuring-dnssec.html)
- [AWS KMS Best Practices](https://docs.aws.amazon.com/kms/latest/developerguide/best-practices.html)
- [CIS AWS Foundations Benchmark](https://www.cisecurity.org/benchmark/amazon_web_services)

### Security Standards & Frameworks

- [OWASP Secure Coding Practices](https://owasp.org/www-project-secure-coding-practices-quick-reference-guide/)
- [NIST Cybersecurity Framework](https://www.nist.gov/cyberframework)
- [NIST 800-171 Rev 3](https://csrc.nist.gov/publications/detail/sp/800-171/rev-3/final)
- [NIST 800-53 Rev 5](https://csrc.nist.gov/publications/detail/sp/800-53/rev-5/final)

### Security Tools

**For Users:**
- [AWS IAM Policy Simulator](https://policysim.aws.amazon.com/)
- [ScoutSuite](https://github.com/nccgroup/ScoutSuite) - AWS security auditing
- [Prowler](https://github.com/prowler-cloud/prowler) - AWS security assessment

**For Developers:**
- [gosec](https://github.com/securego/gosec) - Go security scanner
- [nancy](https://github.com/sonatype-nexus-community/nancy) - Dependency vulnerability scanner
- [trivy](https://github.com/aquasecurity/trivy) - Docker image scanner
- [golangci-lint](https://golangci-lint.run/) - Go linters aggregator

---

## Acknowledgments

We would like to thank the following security researchers for responsible disclosure:

- *No disclosed vulnerabilities yet*

---

**Last Updated**: 2026-01-27
**Version**: 1.1.0 (v0.13.0)
