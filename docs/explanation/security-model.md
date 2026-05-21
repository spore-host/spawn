# Security Model

Understanding spawn's security architecture and threat model.

## Threat Model

### Assets to Protect

**User credentials:**
- AWS access keys
- SSH private keys
- Application secrets

**Instance data:**
- Source code
- Training data
- Model weights
- Computation results

**AWS resources:**
- EC2 instances (compute costs)
- S3 buckets (data)
- DynamoDB tables (metadata)

### Threat Actors

**External attackers:**
- Attempting to compromise AWS accounts
- Scanning for exposed services
- Exploiting vulnerable instances

**Insider threats:**
- Accidental misconfiguration
- Over-privileged IAM policies
- Leaked credentials

**Supply chain:**
- Compromised dependencies
- Malicious AMIs
- Tampered binaries

## Authentication & Authorization

### User Authentication

**AWS credential sources (in order):**
1. Environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`)
2. Shared credentials file (`~/.aws/credentials`)
3. AWS config file (`~/.aws/config`)
4. IAM role (if running on EC2)
5. ECS task role (if running in container)

**No spawn-specific authentication:**
- spawn uses AWS credentials directly
- No additional username/password
- No spawn-managed API keys

### User Authorization

**IAM permission model:**
```
User → IAM Policy → AWS API → Action
```

**Required permissions:**
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:RunInstances",
      "ec2:TerminateInstances",
      "ec2:DescribeInstances",
      "dynamodb:PutItem",
      "dynamodb:Query",
      "s3:GetObject"
    ],
    "Resource": "*"
  }]
}
```

**Permission validation:**
- spawn does NOT pre-validate permissions
- AWS returns error if permission denied
- User sees error message with required permission

### Instance Authentication

**IAM instance profile:**
- Attached at instance launch
- Provides temporary credentials
- Rotates automatically every 6 hours

**Credential retrieval:**
```bash
# Instance queries metadata service
curl http://169.254.169.254/latest/meta-data/iam/security-credentials/spawn-instance-role

# Returns:
{
  "AccessKeyId": "ASIA...",
  "SecretAccessKey": "...",
  "Token": "...",
  "Expiration": "2026-01-27T18:00:00Z"
}
```

**IMDSv2 (recommended):**
```bash
# Require token for metadata access
spawn launch --metadata-options "HttpTokens=required"

# Prevents SSRF attacks from fetching credentials
```

## Network Security

### Default Configuration

**⚠️ Insecure defaults:**
```
Public IP: Assigned
Security Group: default (allows SSH from 0.0.0.0/0)
VPC: Default VPC
Subnet: Public subnet
```

**Why insecure defaults?**
- Ease of use for quick experiments
- Users can SSH immediately
- Trade-off: Security vs convenience

**Recommendation:** Use restrictive security groups

### Secure Configuration

**Minimal security group:**
```bash
# Create restrictive security group
SG_ID=$(aws ec2 create-security-group \
  --group-name spawn-secure \
  --description "Secure spawn instances")

# Allow SSH only from your IP
MY_IP=$(curl -s ifconfig.me)
aws ec2 authorize-security-group-ingress \
  --group-id $SG_ID \
  --protocol tcp \
  --port 22 \
  --cidr $MY_IP/32

# Launch with secure group
spawn launch --security-groups $SG_ID
```

**Private subnet:**
```bash
# Launch in private subnet (no public IP)
spawn launch \
  --subnet subnet-private-xxx \
  --no-public-ip \
  --security-groups $SG_ID

# Access via bastion or Session Manager
spawn connect i-0abc123 --ssm
```

### Network Isolation

**VPC isolation:**
- Each AWS account has separate VPCs
- Instances in different VPCs cannot communicate by default
- Use VPC peering for controlled cross-VPC access

**Security group isolation:**
- Group instances by sensitivity level
- Dev instances: Allow SSH from anywhere
- Prod instances: Allow SSH only from bastion

## Secrets Management

### Anti-Patterns (DON'T DO THIS)

**❌ Hardcoded in user-data:**
```bash
spawn launch --user-data "
  export API_KEY=sk-abc123xyz789
  python app.py
"
# Secrets visible in:
# - EC2 console (user-data)
# - AWS CloudTrail logs
# - DynamoDB spawn-instances table
```

**❌ Passed via environment:**
```bash
spawn launch --user-data "
  export DATABASE_PASSWORD=\$DB_PASSWORD
  python app.py
"
# Better than hardcoded, but still visible in metadata
```

### Secure Patterns

**✅ AWS Secrets Manager:**
```bash
# Store secret
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

**✅ SSM Parameter Store:**
```bash
# Store encrypted parameter
aws ssm put-parameter \
  --name /myapp/api-key \
  --value "sk-abc123xyz789" \
  --type SecureString

# Launch with SSM access
spawn launch \
  --iam-policy ssm:ReadOnly \
  --user-data "
    API_KEY=\$(aws ssm get-parameter \
      --name /myapp/api-key \
      --with-decryption \
      --query Parameter.Value \
      --output text)
    export API_KEY
    python app.py
  "
```

## Data Protection

### Data at Rest

**EBS volumes:**
```bash
# Enable encryption
spawn launch --encrypt-volumes

# Uses default AWS KMS key (aws/ebs)
# Or specify custom key:
spawn launch --encrypt-volumes --kms-key-id arn:aws:kms:...
```

**S3 buckets:**
```bash
# Server-side encryption (default)
aws s3 cp file.txt s3://bucket/  # Encrypted automatically

# Client-side encryption
aws s3 cp file.txt s3://bucket/ --sse aws:kms --sse-kms-key-id <key-id>
```

**DynamoDB:**
```bash
# Encryption at rest enabled by default
# Uses AWS-managed KMS key
```

### Data in Transit

**SSH (instance access):**
- Encrypted by default (SSH protocol)
- Key-based authentication (no passwords)

**AWS API calls:**
- HTTPS by default
- TLS 1.2+ required
- Certificate validation enabled

**S3 transfers:**
- HTTPS by default
- Use `--no-verify-ssl` only for debugging (insecure)

## IAM Policy Hardening

### Least Privilege Principle

**❌ Overly permissive:**
```json
{
  "Effect": "Allow",
  "Action": "ec2:*",
  "Resource": "*"
}
```

**✅ Scoped to actions:**
```json
{
  "Effect": "Allow",
  "Action": [
    "ec2:RunInstances",
    "ec2:TerminateInstances",
    "ec2:DescribeInstances"
  ],
  "Resource": "*"
}
```

**✅ Scoped to resources:**
```json
{
  "Effect": "Allow",
  "Action": "s3:GetObject",
  "Resource": "arn:aws:s3:::spawn-binaries-*/*"
}
```

### Instance IAM Policy Templates

**spawn built-in templates:**
```bash
# Read-only S3 access (specific bucket)
spawn launch --iam-policy s3:ReadOnly

# Expands to:
{
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:ListBucket"],
  "Resource": [
    "arn:aws:s3:::spawn-binaries-*/*",
    "arn:aws:s3:::spawn-results-*/*"
  ]
}
```

**Custom policies:**
```bash
# Provide custom policy file
spawn launch --iam-policy-file my-policy.json
```

### Policy Conditions

**Restrict by time:**
```json
{
  "Effect": "Allow",
  "Action": "s3:PutObject",
  "Resource": "arn:aws:s3:::results/*",
  "Condition": {
    "DateGreaterThan": {"aws:CurrentTime": "2026-01-01T00:00:00Z"},
    "DateLessThan": {"aws:CurrentTime": "2026-12-31T23:59:59Z"}
  }
}
```

**Restrict by source IP:**
```json
{
  "Effect": "Allow",
  "Action": "ec2:RunInstances",
  "Resource": "*",
  "Condition": {
    "IpAddress": {"aws:SourceIp": "203.0.113.0/24"}
  }
}
```

## Audit Logging

### What Gets Logged

**CloudTrail (AWS API calls):**
- `RunInstances` - Who launched what instance
- `TerminateInstances` - Who terminated instance
- `PutObject` (S3) - What was uploaded
- `GetSecretValue` - Which secrets were accessed

**spored logs (on instance):**
- TTL expiration events
- Idle detection triggers
- Spot interruption notices
- DNS registration/deregistration

**DynamoDB (spawn metadata):**
- Instance launch parameters
- Launch timestamp
- User tags

### Enabling Audit Logging

**CloudTrail:**
```bash
# Create trail
aws cloudtrail create-trail \
  --name spawn-audit \
  --s3-bucket-name my-audit-logs

# Enable logging
aws cloudtrail start-logging --name spawn-audit
```

**CloudWatch Logs (spored):**
```bash
spawn launch --user-data "
  # Install CloudWatch agent
  sudo yum install -y amazon-cloudwatch-agent

  # Configure to ship spored logs
  # ...
"
```

### Querying Audit Logs

**CloudTrail:**
```bash
# Find all instances launched by user
aws cloudtrail lookup-events \
  --lookup-attributes \
    AttributeKey=Username,AttributeValue=alice \
    AttributeKey=EventName,AttributeValue=RunInstances
```

**DynamoDB:**
```bash
# Find all instances with tag
aws dynamodb query \
  --table-name spawn-instances \
  --index-name tag-index \
  --key-condition-expression "tag_key = :key AND tag_value = :value" \
  --expression-attribute-values '{
    ":key": {"S": "owner"},
    ":value": {"S": "alice"}
  }'
```

## Compliance

### HIPAA

**Requirements:**
1. Sign AWS BAA (Business Associate Agreement)
2. Use HIPAA-eligible services only (EC2, S3, DynamoDB are eligible)
3. Encrypt all data (EBS, S3)
4. Enable CloudTrail audit logging
5. Implement access controls (MFA, least privilege)

**spawn configuration:**
```bash
spawn launch \
  --encrypt-volumes \
  --kms-key-id <hipaa-compliant-key> \
  --iam-policy <least-privilege-policy> \
  --no-public-ip \
  --subnet <private-subnet> \
  --tags compliance=hipaa,data-class=phi
```

### PCI DSS

**Requirements:**
1. Network segmentation (separate VPC/subnets)
2. Encryption (TLS 1.2+, AES-256)
3. Access controls (MFA, audit logs)
4. Vulnerability scanning (quarterly)

**spawn configuration:**
```bash
# Dedicated PCI VPC
spawn launch \
  --vpc vpc-pci \
  --subnet subnet-pci-private \
  --security-groups sg-pci \
  --encrypt-volumes \
  --tags compliance=pci-dss,cardholder-data=yes
```

### SOC 2

**Requirements:**
1. Access control policies
2. Encryption (data at rest and in transit)
3. Audit logging (all administrative actions)
4. Change management

**spawn alignment:**
- ✅ IAM-based access control
- ✅ Optional EBS encryption
- ✅ CloudTrail audit logs
- ⚠️ Manual change management (no built-in)

## Incident Response

### Compromised Credentials

**If AWS credentials leaked:**
```bash
# 1. Immediately deactivate
aws iam update-access-key \
  --access-key-id AKIA... \
  --status Inactive

# 2. Generate new keys
aws iam create-access-key --user-name spawn-user

# 3. Audit usage
aws cloudtrail lookup-events \
  --lookup-attributes \
    AttributeKey=AccessKeyId,AttributeValue=AKIA...

# 4. Terminate suspicious instances
spawn list --filter "launch_time > <leak_time>"
spawn cancel <suspicious-instances>
```

### Compromised Instance

**If instance compromised:**
```bash
# 1. Isolate instance (deny all traffic)
INSTANCE_ID=i-0abc123
ISOLATION_SG=$(aws ec2 create-security-group \
  --group-name isolation-$(date +%s) \
  --description "Isolation group")

aws ec2 revoke-security-group-egress \
  --group-id $ISOLATION_SG \
  --ip-permissions IpProtocol=-1,IpRanges='[{CidrIp=0.0.0.0/0}]'

aws ec2 modify-instance-attribute \
  --instance-id $INSTANCE_ID \
  --groups $ISOLATION_SG

# 2. Create forensic snapshot
SNAPSHOT_ID=$(aws ec2 create-snapshot \
  --volume-id <volume-id> \
  --description "Forensic snapshot")

# 3. Terminate instance
spawn cancel $INSTANCE_ID

# 4. Review CloudTrail
aws cloudtrail lookup-events --start-time <timestamp>
```

## Security Best Practices

### For Users

**1. Use restrictive security groups**
```bash
spawn launch --security-groups sg-restrictive
```

**2. Enable EBS encryption**
```bash
spawn launch --encrypt-volumes
```

**3. Use Secrets Manager for secrets**
```bash
# Never hardcode secrets
```

**4. Enable IMDSv2**
```bash
spawn launch --metadata-options "HttpTokens=required"
```

**5. Use private subnets for sensitive workloads**
```bash
spawn launch --subnet subnet-private --no-public-ip
```

**6. Tag instances with owner**
```bash
spawn launch --tags owner=alice,project=ml
```

**7. Monitor CloudTrail regularly**
```bash
# Set up CloudWatch alarms for suspicious activity
```

### For Administrators

**1. Enforce tagging policies (SCPs)**
```json
{
  "Effect": "Deny",
  "Action": "ec2:RunInstances",
  "Resource": "arn:aws:ec2:*:*:instance/*",
  "Condition": {
    "StringNotEquals": {"aws:RequestTag/spawn": "true"}
  }
}
```

**2. Restrict regions (SCPs)**
```json
{
  "Effect": "Deny",
  "Action": "*",
  "Resource": "*",
  "Condition": {
    "StringNotEquals": {
      "aws:RequestedRegion": ["us-east-1", "us-west-2"]
    }
  }
}
```

**3. Enable CloudTrail in all accounts**

**4. Regular security audits**
- Review IAM policies quarterly
- Scan for overly permissive security groups
- Check for unencrypted EBS volumes

**5. Automated compliance checking**
```bash
# AWS Config rules
aws configservice put-config-rule \
  --config-rule '{
    "ConfigRuleName": "ec2-ebs-encryption-by-default",
    "Source": {
      "Owner": "AWS",
      "SourceIdentifier": "EC2_EBS_ENCRYPTION_BY_DEFAULT"
    }
  }'
```

## Related Documentation

- [How-To: Security & IAM](../how-to/security-iam.md) - Security recipes
- [How-To: Multi-Account Setup](../how-to/multi-account.md) - Account isolation
- [Architecture Overview](architecture.md) - System design
- [IAM Policies Reference](../reference/iam-policies.md) - Policy templates
