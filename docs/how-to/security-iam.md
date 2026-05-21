# How-To: Security & IAM

Security best practices and IAM configuration for spawn.

## Principle of Least Privilege

### Problem
Giving instances too many permissions creates security risk.

### Solution: Minimal IAM Policies

**Bad - Overly permissive:**
```bash
spawn launch --iam-policy s3:FullAccess,dynamodb:FullAccess
# Can access ALL S3 buckets and DynamoDB tables
```

**Good - Scoped to specific resources:**
```bash
# Create custom policy
cat > policy.json << 'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject"
      ],
      "Resource": "arn:aws:s3:::my-specific-bucket/data/*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:PutItem",
        "dynamodb:GetItem"
      ],
      "Resource": "arn:aws:dynamodb:us-east-1:123456789012:table/my-table"
    }
  ]
}
EOF

spawn launch --iam-policy-file policy.json
```

---

## IAM Policy Templates

### spawn Built-In Templates

**Read-only templates:**
```bash
spawn launch --iam-policy s3:ReadOnly,dynamodb:ReadOnly
```

**Write-only templates:**
```bash
spawn launch --iam-policy s3:WriteOnly,logs:WriteOnly
```

**Full access (use sparingly):**
```bash
spawn launch --iam-policy s3:FullAccess
```

### Available Templates

| Template | Permissions |
|----------|-------------|
| `s3:ReadOnly` | s3:GetObject, s3:ListBucket |
| `s3:WriteOnly` | s3:PutObject |
| `s3:FullAccess` | s3:* |
| `dynamodb:ReadOnly` | dynamodb:GetItem, Query, Scan |
| `dynamodb:WriteOnly` | dynamodb:PutItem, UpdateItem |
| `dynamodb:FullAccess` | dynamodb:* |
| `logs:WriteOnly` | logs:CreateLogGroup, CreateLogStream, PutLogEvents |
| `ecr:ReadOnly` | Pull Docker images from ECR |
| `sqs:ReadOnly` | Receive messages from SQS |
| `sqs:WriteOnly` | Send messages to SQS |
| `secretsmanager:ReadOnly` | Get secrets |
| `ssm:ReadOnly` | Get SSM parameters |

---

## Secrets Management

### Never Hardcode Secrets

**Bad:**
```bash
# DON'T DO THIS
spawn launch --user-data "
  export API_KEY=sk-abc123xyz789
  python app.py
"
```

**Good - Use AWS Secrets Manager:**
```bash
# Store secret
aws secretsmanager create-secret \
  --name myapp/api-key \
  --secret-string "sk-abc123xyz789"

# Launch with Secrets Manager access
spawn launch \
  --iam-policy secretsmanager:ReadOnly \
  --user-data "
    API_KEY=$(aws secretsmanager get-secret-value \
      --secret-id myapp/api-key \
      --query SecretString \
      --output text)
    export API_KEY
    python app.py
  "
```

**Good - Use SSM Parameter Store:**
```bash
# Store parameter (encrypted)
aws ssm put-parameter \
  --name /myapp/api-key \
  --value "sk-abc123xyz789" \
  --type SecureString

# Launch with SSM access
spawn launch \
  --iam-policy ssm:ReadOnly \
  --user-data "
    API_KEY=$(aws ssm get-parameter \
      --name /myapp/api-key \
      --with-decryption \
      --query Parameter.Value \
      --output text)
    export API_KEY
    python app.py
  "
```

---

## Network Security

### Security Groups

**Principle: Least privilege network access**

```bash
# Create restrictive security group
SG_ID=$(aws ec2 create-security-group \
  --group-name spawn-secure \
  --description "Secure spawn instances" \
  --vpc-id vpc-xxx \
  --query 'GroupId' \
  --output text)

# Allow SSH only from your IP
MY_IP=$(curl -s ifconfig.me)
aws ec2 authorize-security-group-ingress \
  --group-id $SG_ID \
  --protocol tcp \
  --port 22 \
  --cidr $MY_IP/32

# Allow HTTPS outbound only
aws ec2 authorize-security-group-egress \
  --group-id $SG_ID \
  --protocol tcp \
  --port 443 \
  --cidr 0.0.0.0/0

# Remove default allow-all outbound rule
aws ec2 revoke-security-group-egress \
  --group-id $SG_ID \
  --ip-permissions IpProtocol=-1,IpRanges='[{CidrIp=0.0.0.0/0}]'

# Launch with secure group
spawn launch --security-groups $SG_ID
```

### Private Subnets

**For sensitive workloads, use private subnets:**

```bash
# Launch in private subnet (no public IP)
spawn launch \
  --subnet subnet-private-xxx \
  --no-public-ip \
  --security-groups $SG_ID
```

**Access via bastion or Session Manager:**
```bash
spawn connect <instance-id> --bastion <bastion-id>
# or
spawn connect <instance-id> --ssm
```

---

## Encryption

### EBS Volume Encryption

**Always encrypt data at rest:**

```bash
spawn launch --encrypt-volumes
```

**Use custom KMS key:**
```bash
# Create KMS key
KMS_KEY=$(aws kms create-key \
  --description "spawn instance volumes" \
  --query 'KeyMetadata.KeyId' \
  --output text)

spawn launch --encrypt-volumes --kms-key-id $KMS_KEY
```

### Data in Transit

**Use TLS/SSL for all data transfers:**

```bash
# HTTPS for S3
aws s3 cp file.txt s3://bucket/file.txt  # Uses HTTPS by default

# SSH for file transfer
scp file.txt ec2-user@<ip>:/path/  # Encrypted

# VPN for internal traffic
# Use AWS Client VPN or Site-to-Site VPN
```

---

## Audit Logging

### Enable CloudTrail

**Track all API calls:**

```bash
# Create CloudTrail trail (one-time setup)
aws cloudtrail create-trail \
  --name spawn-audit \
  --s3-bucket-name my-audit-logs

# Enable logging
aws cloudtrail start-logging --name spawn-audit
```

**Monitor spawn actions:**
```bash
# Query CloudTrail logs
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=ResourceName,AttributeValue=spawn \
  --max-results 50
```

### Instance Logs

**Ship logs to CloudWatch:**

```bash
spawn launch --user-data "
  # Install CloudWatch agent
  sudo yum install -y amazon-cloudwatch-agent

  # Configure agent
  sudo /opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl \
    -a fetch-config \
    -m ec2 \
    -s \
    -c file:/opt/aws/amazon-cloudwatch-agent/etc/config.json
"
```

---

## SSH Hardening

### Disable Password Authentication

**On custom AMI, configure SSH:**

```bash
# /etc/ssh/sshd_config
PasswordAuthentication no
ChallengeResponseAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
```

### Use SSH Certificates

**For organizations:**

```bash
# Setup SSH CA (one-time)
# See: https://smallstep.com/blog/use-ssh-certificates/

# Launch instance with CA public key
spawn launch --user-data "
  echo '<ca-public-key>' | sudo tee -a /etc/ssh/ca-keys.pub
  echo 'TrustedUserCAKeys /etc/ssh/ca-keys.pub' | sudo tee -a /etc/ssh/sshd_config
  sudo systemctl restart sshd
"
```

### SSH Key Rotation

**Rotate keys regularly:**

```bash
# Generate new key pair
ssh-keygen -t ed25519 -f ~/.ssh/spawn-2026 -N ""

# Create new key pair in AWS
aws ec2 import-key-pair \
  --key-name spawn-2026 \
  --public-key-material fileb://~/.ssh/spawn-2026.pub

# Launch new instances with new key
spawn launch --key-pair spawn-2026

# After migration, delete old key
aws ec2 delete-key-pair --key-name spawn-2025
```

---

## IMDSv2 (Instance Metadata Service v2)

### Require IMDSv2

**Prevent SSRF attacks:**

```bash
spawn launch --metadata-options "HttpTokens=required,HttpPutResponseHopLimit=1"
```

**What this does:**
- Requires authentication token for metadata requests
- Prevents SSRF attacks from fetching instance credentials

**In application code:**
```python
import boto3
import requests

# Get IMDSv2 token
token_url = "http://169.254.169.254/latest/api/token"
token_response = requests.put(token_url, headers={"X-aws-ec2-metadata-token-ttl-seconds": "21600"})
token = token_response.text

# Use token to fetch metadata
metadata_url = "http://169.254.169.254/latest/meta-data/instance-id"
response = requests.get(metadata_url, headers={"X-aws-ec2-metadata-token": token})
instance_id = response.text
```

---

## Resource Tagging for Security

### Tag All Resources

**Enforce tagging policy:**

```bash
spawn launch \
  --tags owner=alice,project=ml-research,classification=internal,data-class=confidential
```

**Use tags for access control:**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "ec2:TerminateInstances",
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/owner": "${aws:username}"
        }
      }
    }
  ]
}
```

**Users can only terminate their own instances.**

---

## Security Scanning

### Vulnerability Scanning

**Use AWS Inspector:**

```bash
# Create assessment target
aws inspector create-assessment-target \
  --assessment-target-name spawn-instances \
  --resource-group-arn arn:aws:inspector:us-east-1:123456789012:resourcegroup/0-xxx

# Run assessment
aws inspector start-assessment-run \
  --assessment-template-arn arn:aws:inspector:us-east-1:123456789012:target/0-xxx/template/0-yyy
```

### Container Scanning

**If using Docker, scan images:**

```bash
# Scan Docker image before deploying
docker scan myimage:latest

# Or use Trivy
trivy image myimage:latest
```

---

## Compliance

### HIPAA Compliance

**For HIPAA workloads:**

1. **Sign BAA with AWS** (Business Associate Agreement)
2. **Use HIPAA-eligible services only**
3. **Encrypt all data** (at rest and in transit)
4. **Enable CloudTrail** for audit logging
5. **Implement access controls** (MFA, least privilege)

```bash
spawn launch \
  --encrypt-volumes \
  --kms-key-id <hipaa-compliant-kms-key> \
  --iam-policy <least-privilege-policy> \
  --no-public-ip \
  --subnet <private-subnet> \
  --tags compliance=hipaa,data-class=phi
```

### PCI DSS Compliance

**For payment card data:**

1. **Network segmentation** (separate VPC/subnets)
2. **Encryption** (TLS 1.2+, AES-256)
3. **Access controls** (MFA, audit logs)
4. **Vulnerability scanning** (quarterly)

---

## Incident Response

### Isolate Compromised Instance

```bash
#!/bin/bash
# isolate-instance.sh <instance-id>

INSTANCE_ID=$1

echo "Isolating instance $INSTANCE_ID..."

# Create isolation security group (deny all)
ISOLATION_SG=$(aws ec2 create-security-group \
  --group-name isolation-$(date +%s) \
  --description "Isolation group for incident response" \
  --query 'GroupId' \
  --output text)

# Remove all rules (deny all traffic)
aws ec2 revoke-security-group-egress \
  --group-id $ISOLATION_SG \
  --ip-permissions IpProtocol=-1,IpRanges='[{CidrIp=0.0.0.0/0}]'

# Apply isolation group to instance
aws ec2 modify-instance-attribute \
  --instance-id $INSTANCE_ID \
  --groups $ISOLATION_SG

echo "Instance isolated. No network traffic allowed."

# Create forensic snapshot
SNAPSHOT_ID=$(aws ec2 create-snapshot \
  --volume-id $(aws ec2 describe-instances --instance-ids $INSTANCE_ID \
    --query 'Reservations[0].Instances[0].BlockDeviceMappings[0].Ebs.VolumeId' \
    --output text) \
  --description "Forensic snapshot of $INSTANCE_ID" \
  --query 'SnapshotId' \
  --output text)

echo "Forensic snapshot created: $SNAPSHOT_ID"

# Tag for investigation
aws ec2 create-tags \
  --resources $INSTANCE_ID $SNAPSHOT_ID \
  --tags Key=incident-response,Value=true Key=isolated,Value=true

echo "Incident response complete"
```

---

## Security Checklist

### Before Launch

- [ ] Use least-privilege IAM policies
- [ ] Enable EBS encryption
- [ ] Use private subnet if possible
- [ ] Restrictive security group rules
- [ ] IMDSv2 required
- [ ] Tag with owner and classification

### After Launch

- [ ] Verify security group rules
- [ ] Check IAM role permissions
- [ ] Review CloudTrail logs
- [ ] Enable CloudWatch logging
- [ ] Run vulnerability scan

### Ongoing

- [ ] Rotate SSH keys quarterly
- [ ] Review IAM policies monthly
- [ ] Update AMIs with security patches
- [ ] Monitor CloudTrail for anomalies
- [ ] Clean up old/unused resources

---

## Common Security Mistakes

### ❌ Don't

1. **Hardcode secrets in user data or code**
2. **Use overly permissive IAM policies** (`*:*`)
3. **Open security groups to 0.0.0.0/0** (except HTTP/HTTPS)
4. **Store unencrypted sensitive data**
5. **Share SSH private keys**
6. **Use same key for all environments**
7. **Ignore CloudTrail alerts**
8. **Run as root** (use ec2-user)

### ✅ Do

1. **Use AWS Secrets Manager/SSM for secrets**
2. **Scope IAM to specific resources**
3. **Restrict security groups to known IPs**
4. **Encrypt EBS volumes**
5. **Use unique SSH keys per user**
6. **Separate keys for dev/prod**
7. **Monitor CloudTrail**
8. **Run as non-root user**

---

## See Also

- [IAM Policies Reference](../reference/iam-policies.md) - Policy templates
- [spawn launch](../reference/commands/launch.md) - Security flags
- [AWS Security Best Practices](https://aws.amazon.com/architecture/security-identity-compliance/)
- [AWS Well-Architected Framework](https://aws.amazon.com/architecture/well-architected/)
