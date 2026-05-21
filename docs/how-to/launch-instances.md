# How-To: Launch Instances

Quick recipes for common instance launching scenarios.

## Quick Launch

### Development Instance

```bash
spawn launch --instance-type t3.micro --ttl 4h
```

**Use for:** Testing, learning, development

### Production Instance

```bash
spawn launch --instance-type m7i.large --ttl 24h --tags env=prod
```

**Use for:** Production workloads, stable services

### GPU Instance

```bash
spawn launch --instance-type g5.xlarge --ttl 12h --idle-timeout 1h
```

**Use for:** ML training, rendering

## Launch with Specific Configuration

### Custom AMI

```bash
spawn launch --instance-type t3.micro --ami ami-0abc123def456789
```

### Custom Region

```bash
spawn launch --instance-type t3.micro --region us-west-2
```

### With Tags

```bash
spawn launch --instance-type t3.micro \
  --tags env=dev,project=myapp,team=backend
```

### With Name

```bash
spawn launch --instance-type t3.micro --name my-dev-instance
```

## Network Configuration

### Public IP (Default)

```bash
spawn launch --instance-type t3.micro
```

### Private IP Only

```bash
spawn launch --instance-type t3.micro --no-public-ip
```

### Custom Security Group

```bash
spawn launch --instance-type t3.micro --security-groups sg-0123456789abc
```

### Custom VPC/Subnet

```bash
spawn launch --instance-type t3.micro \
  --vpc vpc-xxx \
  --subnet subnet-xxx
```

## Storage Configuration

### Custom Volume Size

```bash
spawn launch --instance-type t3.micro --volume-size 100
```

**Default:** 8 GB

### Encrypted Volume

```bash
spawn launch --instance-type t3.micro --encrypt-volumes
```

### Custom Volume Type

```bash
spawn launch --instance-type t3.micro --volume-type io2
```

**Types:** `gp3` (default), `gp2`, `io1`, `io2`, `st1`, `sc1`

## Lifecycle Configuration

### With TTL

```bash
spawn launch --instance-type t3.micro --ttl 8h
```

### With Idle Timeout

```bash
spawn launch --instance-type t3.micro --idle-timeout 1h
```

### Hibernate on Idle

```bash
spawn launch --instance-type m7i.large --idle-timeout 1h --hibernate-on-idle
```

### On Completion

```bash
spawn launch --instance-type t3.micro --on-complete terminate --ttl 4h
```

**On instance:** `spored complete` to trigger termination

## IAM Configuration

### With Policy Templates

```bash
spawn launch --instance-type t3.micro --iam-policy s3:ReadOnly,logs:WriteOnly
```

### With Custom Policy File

```bash
spawn launch --instance-type t3.micro --iam-policy-file ./policy.json
```

### With AWS Managed Policy

```bash
spawn launch --instance-type t3.micro \
  --iam-managed-policies arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

## Spot Instances

### Basic Spot

```bash
spawn launch --instance-type t3.micro --spot
```

### With Max Price

```bash
spawn launch --instance-type t3.micro --spot --spot-max-price 0.05
```

### Spot with Hibernation

```bash
spawn launch --instance-type m7i.large --spot --hibernate \
  --spot-interruption-behavior hibernate
```

## User Data Scripts

### Inline Script

```bash
spawn launch --instance-type t3.micro \
  --user-data "#!/bin/bash
yum install -y docker
systemctl start docker"
```

### From File

```bash
spawn launch --instance-type t3.micro --user-data @setup.sh
```

**setup.sh:**
```bash
#!/bin/bash
yum update -y
yum install -y docker git
systemctl start docker
usermod -aG docker ec2-user
```

## Wait for Ready

### Wait for Running

```bash
spawn launch --instance-type t3.micro --wait-for-running
```

### Wait for SSH

```bash
spawn launch --instance-type t3.micro --wait-for-ssh
```

### Both

```bash
spawn launch --instance-type t3.micro --wait-for-ssh
```

## Output Options

### JSON Output

```bash
INSTANCE_ID=$(spawn launch --instance-type t3.micro --json | jq -r '.instance_id')
```

### Quiet (ID Only)

```bash
INSTANCE_ID=$(spawn launch --instance-type t3.micro --quiet)
```

### Save Instance ID

```bash
spawn launch --instance-type t3.micro --output-id instance-id.txt
```

## Complete Examples

### Web Server

```bash
spawn launch \
  --instance-type t3.medium \
  --name web-server \
  --ttl 24h \
  --tags env=prod,app=web \
  --iam-policy s3:ReadOnly \
  --user-data @setup-web.sh
```

**setup-web.sh:**
```bash
#!/bin/bash
yum install -y httpd
systemctl start httpd
systemctl enable httpd
aws s3 sync s3://my-bucket/website/ /var/www/html/
```

### ML Training

```bash
spawn launch \
  --instance-type g5.xlarge \
  --name ml-training \
  --ami ami-pytorch \
  --ttl 12h \
  --idle-timeout 1h \
  --iam-policy s3:FullAccess,logs:WriteOnly \
  --volume-size 500 \
  --tags project=ml,experiment=v2
```

### Batch Processing

```bash
spawn launch \
  --instance-type c7i.xlarge \
  --name batch-job \
  --on-complete terminate \
  --ttl 4h \
  --iam-policy s3:ReadOnly,dynamodb:WriteOnly \
  --user-data @process.sh
```

**process.sh:**
```bash
#!/bin/bash
cd /app
python process.py
spored complete --status success
```

## Common Issues

### "Insufficient capacity"

**Try different AZ:**
```bash
spawn launch --instance-type t3.micro --az us-east-1b
```

### "InvalidKeyPair.NotFound"

**Create key:**
```bash
ssh-keygen -t rsa -f ~/.ssh/id_rsa -N ""
```

### "UnauthorizedOperation"

**Check IAM permissions:**
```bash
aws sts get-caller-identity
```

See [IAM_PERMISSIONS.md](../../IAM_PERMISSIONS.md)

## See Also

- [Tutorial 2: Your First Instance](../tutorials/02-first-instance.md) - Detailed walkthrough
- [Command Reference: launch](../reference/commands/launch.md) - All flags
- [How-To: Spot Instances](spot-instances.md) - Spot instance guide
