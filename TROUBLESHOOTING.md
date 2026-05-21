# Troubleshooting Guide

**Table of Contents**
- [Common Errors](#common-errors)
- [Launch Issues](#launch-issues)
- [Slurm-Specific Issues](#slurm-specific-issues)
- [Data Staging Issues](#data-staging-issues)
- [MPI Issues](#mpi-issues)
- [DNS Issues](#dns-issues)
- [Job Array Issues](#job-array-issues)
- [Spot Instance Issues](#spot-instance-issues)
- [Networking Issues](#networking-issues)
- [Diagnostic Commands](#diagnostic-commands)
- [Getting Help](#getting-help)

---

## Common Errors

### Quota/Capacity Errors

#### InsufficientInstanceCapacity

**Error:**
```
Error: InsufficientInstanceCapacity
We currently do not have sufficient c5.4xlarge capacity in the us-east-1a
```

**Cause:** AWS temporarily out of capacity for the requested instance type in that AZ.

**Solutions:**

1. **Try different AZ:**
```bash
spawn launch --instance-type c5.4xlarge --az us-east-1b
```

2. **Try different region:**
```bash
spawn launch --instance-type c5.4xlarge --region us-west-2
```

3. **Use spot instances (more availability):**
```bash
spawn launch --instance-type c5.4xlarge --spot
```

4. **Try similar instance type:**
```bash
# If c5.4xlarge unavailable, try:
spawn launch --instance-type c5n.4xlarge
spawn launch --instance-type m5.4xlarge
```

5. **Use `--auto-region` for automatic failover:**
```bash
spawn launch --params sweep.yaml --auto-region
# Automatically tries multiple regions until capacity found
```

---

#### VcpuLimitExceeded

**Error:**
```
Error: VcpuLimitExceeded
You have exceeded your limit of 128 vCPUs in us-east-1
```

**Cause:** AWS account vCPU quota exceeded.

**Solutions:**

1. **Check current usage:**
```bash
spawn quota
```

Output:
```
Region: us-east-1
vCPU Limit: 128
vCPU Used: 128 (100%)
Available: 0

Running Instances:
  c5.4xlarge (16 vCPUs) × 8 = 128 vCPUs
```

2. **Request quota increase:**
```bash
# Request via AWS Console
# Service Quotas → EC2 → Running On-Demand instances

# Or via CLI
aws service-quotas request-service-quota-increase \
  --service-code ec2 \
  --quota-code L-1216C47A \
  --desired-value 256 \
  --region us-east-1
```

3. **Use different region:**
```bash
spawn launch --params sweep.yaml --region us-west-2
```

4. **Terminate unused instances:**
```bash
# List all running instances
spawn list

# Terminate specific instance
spawn terminate i-1234567890abcdef0

# Terminate old instances
spawn cleanup --older-than 24h
```

5. **Use smaller instance types:**
```bash
# Instead of c5.4xlarge (16 vCPUs)
spawn launch --instance-type c5.2xlarge  # 8 vCPUs
```

---

### Permission Errors

#### AccessDenied (EC2)

**Error:**
```
Error: AccessDeniedException
User: arn:aws:iam::123456789012:user/myuser is not authorized to perform: ec2:RunInstances
```

**Cause:** IAM user lacks EC2 permissions.

**Solutions:**

1. **Check current permissions:**
```bash
aws iam get-user-policy \
  --user-name myuser \
  --policy-name spawn-policy
```

2. **Required minimum permissions:**
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:DescribeInstances",
        "ec2:TerminateInstances",
        "ec2:CreateTags",
        "ec2:DescribeRegions",
        "ec2:DescribeKeyPairs",
        "ec2:ImportKeyPair"
      ],
      "Resource": "*"
    }
  ]
}
```

3. **Attach policy:**
```bash
# Create policy
aws iam put-user-policy \
  --user-name myuser \
  --policy-name spawn-ec2-policy \
  --policy-document file://spawn-policy.json

# Or use managed policy
aws iam attach-user-policy \
  --user-name myuser \
  --policy-arn arn:aws:iam::aws:policy/AmazonEC2FullAccess
```

4. **Test permissions:**
```bash
# Dry-run launch
aws ec2 run-instances \
  --dry-run \
  --image-id ami-12345678 \
  --instance-type t3.micro \
  --region us-east-1
```

---

#### AccessDenied (S3)

**Error:**
```
Error: Access Denied
When calling the PutObject operation: Access Denied
```

**Cause:** Missing S3 permissions for data staging or result collection.

**Solutions:**

1. **Required S3 permissions:**
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:ListBucket",
        "s3:DeleteObject"
      ],
      "Resource": [
        "arn:aws:s3:::spawn-staging-*/*",
        "arn:aws:s3:::spawn-results-*/*"
      ]
    }
  ]
}
```

2. **Check bucket permissions:**
```bash
aws s3 ls s3://spawn-staging-us-east-1/
# Should list objects or return empty (not Access Denied)
```

3. **Instance role for automatic downloads:**
```bash
spawn launch --params sweep.yaml \
  --iam-policy s3:GetObject \
  --iam-policy s3:PutObject
```

---

### Network Errors

#### RequestLimitExceeded

**Error:**
```
Error: RequestLimitExceeded
Request limit exceeded
```

**Cause:** Too many API calls to AWS in short period (rate limiting).

**Solutions:**

1. **Reduce parallelism:**
```bash
# Instead of launching 1000 instances at once
spawn launch --params sweep.yaml --max-concurrent 50
```

2. **Add launch delay:**
```bash
spawn launch --params sweep.yaml --launch-delay 100ms
```

3. **Use exponential backoff (automatic):**
```bash
# spawn automatically retries with backoff
# Check logs for retry attempts
spawn launch --params sweep.yaml --verbose
```

4. **Batch operations:**
```bash
# Instead of individual launches
# Use parameter sweeps
spawn launch --params sweep.yaml
```

---

## Launch Issues

### Instance Launch Failures

#### InvalidAMIID.NotFound

**Error:**
```
Error: InvalidAMIID.NotFound
The image id '[ami-12345678]' does not exist
```

**Cause:** AMI doesn't exist in the target region or is invalid.

**Solutions:**

1. **Let spawn auto-select AMI:**
```bash
# Remove --ami flag
spawn launch --instance-type t3.micro --region us-east-1
# spawn automatically selects latest AL2023 AMI
```

2. **Find correct AMI for region:**
```bash
# List available AL2023 AMIs
aws ec2 describe-images \
  --owners amazon \
  --filters "Name=name,Values=al2023-ami-2023*" \
  --query 'Images[*].[ImageId,Name,CreationDate]' \
  --output table \
  --region us-east-1
```

3. **Copy AMI to new region:**
```bash
# If using custom AMI
aws ec2 copy-image \
  --source-region us-east-1 \
  --source-image-id ami-12345678 \
  --name "my-custom-ami" \
  --region us-west-2
```

---

#### InvalidKeyPair.NotFound

**Error:**
```
Error: InvalidKeyPair.NotFound
The key pair 'my-key' does not exist
```

**Cause:** SSH key pair doesn't exist in target region.

**Solutions:**

1. **Let spawn auto-create key:**
```bash
# Remove --key-pair flag
spawn launch --instance-type t3.micro
# spawn creates and stores key in ~/.ssh/spawn-key-{region}.pem
```

2. **Import existing key:**
```bash
# Import your public key
spawn key import ~/.ssh/id_rsa.pub --name my-key --region us-east-1
```

3. **Create new key pair:**
```bash
# Create in AWS
aws ec2 create-key-pair \
  --key-name my-key \
  --query 'KeyMaterial' \
  --output text \
  --region us-east-1 > ~/.ssh/my-key.pem

chmod 600 ~/.ssh/my-key.pem
```

4. **List available keys:**
```bash
aws ec2 describe-key-pairs --region us-east-1
```

---

#### InvalidParameterCombination (Hibernation)

**Error:**
```
Error: InvalidParameterCombination
Hibernation is not supported for instance type t3.micro
```

**Cause:** Not all instance types support hibernation.

**Solutions:**

1. **Check hibernation support:**
```bash
# Supported instance families: C3, C4, C5, M3, M4, M5, R3, R4, R5, T2, T3, T3a
# Must have <= 150GB RAM
```

2. **Remove hibernation flag:**
```bash
spawn launch --instance-type t3.micro
# Without --hibernate flag
```

3. **Use supported instance type:**
```bash
spawn launch --instance-type m5.large --hibernate
```

---

### Instance State Issues

#### InstanceNotReady

**Error:**
```
Error: Instance i-1234567890abcdef0 stuck in 'pending' state
```

**Cause:** Instance startup problems (AMI issues, userdata errors, capacity).

**Solutions:**

1. **Check instance status:**
```bash
spawn status i-1234567890abcdef0
```

2. **View system log:**
```bash
aws ec2 get-console-output \
  --instance-id i-1234567890abcdef0 \
  --region us-east-1
```

3. **Check userdata execution:**
```bash
# SSH to instance
spawn ssh i-1234567890abcdef0

# Check userdata log
sudo cat /var/log/cloud-init-output.log
```

4. **Terminate and retry:**
```bash
spawn terminate i-1234567890abcdef0
spawn launch --params sweep.yaml
```

---

## Slurm-Specific Issues

### Parsing Failures

#### InvalidDirective

**Error:**
```
Error: failed to parse Slurm script: unsupported directive: --dependency
```

**Cause:** Slurm directive not supported by spawn.

**Solutions:**

1. **Remove unsupported directive:**
```bash
# Edit script to remove --dependency
vim job.sbatch
```

2. **Use supported workaround:**
```bash
# For dependencies, use sequential launches
sweep1=$(spawn slurm submit job1.sbatch --detach)
spawn wait $sweep1
spawn slurm submit job2.sbatch
```

3. **Check supported directives:**
```bash
spawn slurm convert --help
# Lists all supported #SBATCH directives
```

---

#### ModuleNotFound

**Error:**
```
Error: module: command not found
```

**Cause:** Environment modules system not available in cloud AMI.

**Solutions:**

1. **Use conda instead:**
```bash
# Instead of: module load python/3.11
conda install python=3.11
```

2. **Use custom AMI with modules:**
```bash
#SPAWN --ami=ami-with-modules
```

3. **Direct package installation:**
```bash
# Instead of: module load gcc/11.2
sudo yum install gcc-11.2
```

4. **Create mapping config:**
```yaml
# ~/.spawn/slurm-mapping.yaml
modules:
  "python/3.11":
    setup: "conda install python=3.11"
  "gcc/11.2":
    setup: "sudo yum install gcc"
```

---

### Instance Type Selection

#### NoMatchingInstanceType

**Error:**
```
Error: no instance type matches requirements: 256 vCPUs, 2048GB RAM
```

**Cause:** Requirements exceed available instance types.

**Solutions:**

1. **Check requirements:**
```bash
grep -E "#SBATCH.*(cpus|mem)" job.sbatch
```

2. **Override with custom instance type:**
```bash
spawn slurm submit job.sbatch --instance-type=x1e.32xlarge
```

3. **Use #SPAWN directive:**
```bash
#SPAWN --instance-type=x1e.32xlarge
```

4. **Split into smaller jobs:**
```bash
# Instead of one 256-vCPU job
# Run 16 × 16-vCPU jobs
#SBATCH --cpus-per-task=16
#SBATCH --array=1-16
```

---

## Data Staging Issues

### Upload Failures

#### ConnectionTimeout

**Error:**
```
Error: upload failed: connection timeout after 300s
```

**Cause:** Large file, slow network, or AWS S3 issues.

**Solutions:**

1. **Increase timeout:**
```bash
spawn stage upload large-file.tar.gz --timeout 3600
```

2. **Use multipart upload:**
```bash
# Automatic for files > 5GB
spawn stage upload large-file.tar.gz --multipart
```

3. **Check network:**
```bash
# Test S3 connectivity
aws s3 ls s3://spawn-staging-us-east-1/

# Test upload speed
time aws s3 cp testfile.dat s3://spawn-staging-us-east-1/test/
```

4. **Resume failed upload:**
```bash
spawn stage resume stage-abc123def456
```

---

#### ReplicationError

**Error:**
```
Error: replication failed: access denied in us-west-2
```

**Cause:** Missing permissions in destination region.

**Solutions:**

1. **Check bucket access:**
```bash
aws s3 ls s3://spawn-staging-us-west-2/ --region us-west-2
```

2. **Verify IAM permissions:**
```bash
# Required: s3:PutObject, s3:PutObjectAcl
aws iam get-user-policy \
  --user-name spawn-user \
  --policy-name s3-policy
```

3. **Retry with --force:**
```bash
spawn stage upload file.tar.gz \
  --regions us-east-1,us-west-2 \
  --force
```

---

### Download Failures

#### DownloadAccessDenied

**Error (on instance):**
```
Error: download failed: 403 Forbidden
s3://spawn-staging-us-east-1/stage-abc123/file.tar.gz
```

**Cause:** Instance IAM role lacks S3 GetObject permission.

**Solutions:**

1. **Launch with S3 permissions:**
```bash
spawn launch --params sweep.yaml \
  --iam-policy s3:GetObject \
  --iam-policy s3:ListBucket
```

2. **Use managed policy:**
```bash
spawn launch --params sweep.yaml \
  --iam-managed-policies arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

3. **Check instance role:**
```bash
# On instance
aws sts get-caller-identity
aws s3 ls s3://spawn-staging-us-east-1/
```

---

#### ChecksumMismatch

**Error:**
```
Error: SHA256 checksum mismatch
Expected: a1b2c3d4e5f6...
Got: b2c3d4e5f6a1...
```

**Cause:** File corruption during transfer.

**Solutions:**

1. **Re-download:**
```bash
# On instance
rm /mnt/data/file.tar.gz
aws s3 cp s3://spawn-staging-us-east-1/stage-abc123/file.tar.gz /mnt/data/
```

2. **Verify source file:**
```bash
# On local machine
sha256sum original-file.tar.gz
spawn stage info stage-abc123def456
```

3. **Re-upload:**
```bash
spawn stage delete stage-abc123def456 --yes
spawn stage upload file.tar.gz --regions us-east-1,us-west-2
```

---

## MPI Issues

### Placement Group Errors

#### InvalidPlacementGroup

**Error:**
```
Error: InvalidPlacementGroup.InUse
Placement group 'my-mpi-pg' is in use
```

**Cause:** Placement group has running instances or is being deleted.

**Solutions:**

1. **Use auto-generated placement group:**
```bash
spawn launch --params sweep.yaml --mpi --auto-placement-group
```

2. **Wait for cleanup:**
```bash
# Check placement group status
aws ec2 describe-placement-groups \
  --group-names my-mpi-pg

# Delete if empty
aws ec2 delete-placement-group --group-name my-mpi-pg

# Retry launch
spawn launch --params sweep.yaml --mpi
```

3. **Use different name:**
```bash
spawn launch --params sweep.yaml \
  --mpi \
  --placement-group my-mpi-pg-2
```

---

#### PlacementGroupCapacity

**Error:**
```
Error: InsufficientInstanceCapacity: Insufficient capacity in placement group
```

**Cause:** AWS can't fit all instances in single placement group (cluster strategy).

**Solutions:**

1. **Reduce instance count:**
```bash
# Max ~300 instances in cluster placement group
spawn launch --params sweep.yaml --count 250
```

2. **Use partition placement group:**
```bash
# For > 300 instances
spawn launch --params sweep.yaml \
  --mpi \
  --placement-strategy partition
```

3. **Split across multiple placement groups:**
```bash
# Launch in batches
spawn launch --params sweep1.yaml --count 300 --placement-group pg1
spawn launch --params sweep2.yaml --count 300 --placement-group pg2
```

---

### EFA Issues

#### EFANotSupported

**Error:**
```
Error: EFA not supported on instance type t3.large
```

**Cause:** EFA only available on specific instance types.

**Solutions:**

1. **Use EFA-capable instance:**
```bash
# Supported: c5n, c6gn, p3dn, p4d, p4de
spawn launch --params sweep.yaml \
  --mpi \
  --efa \
  --instance-type c5n.18xlarge
```

2. **Check EFA support:**
```bash
# List EFA-capable types
aws ec2 describe-instance-types \
  --filters "Name=network-info.efa-supported,Values=true" \
  --query 'InstanceTypes[*].InstanceType' \
  --output table
```

3. **Remove EFA if not needed:**
```bash
# Standard MPI without EFA
spawn launch --params sweep.yaml --mpi
```

---

### SSH Key Generation

#### SSHKeyError

**Error:**
```
Error: failed to generate MPI SSH keys
```

**Cause:** SSH key generation or distribution failure.

**Solutions:**

1. **Manual key generation:**
```bash
# Generate key locally
ssh-keygen -t rsa -b 4096 -f ~/.ssh/mpi-key -N ""

# Pass to spawn
spawn launch --params sweep.yaml \
  --mpi \
  --key-pair mpi-key
```

2. **Check userdata logs:**
```bash
# On MPI head node
spawn ssh i-1234567890abcdef0
sudo cat /var/log/cloud-init-output.log | grep -A10 "SSH key"
```

3. **Verify key distribution:**
```bash
# On MPI nodes
cat ~/.ssh/authorized_keys
ls -la ~/.ssh/
```

---

## DNS Issues

### Registration Failures

#### DNSRegistrationFailed

**Error:**
```
Error: DNS registration failed: API endpoint unreachable
```

**Cause:** DNS Lambda API unavailable or network issues.

**Solutions:**

1. **Retry registration:**
```bash
# On instance
/opt/spored/register-dns.sh
```

2. **Check DNS API:**
```bash
# Test endpoint
curl -I https://dns.spore.host/register

# Should return 200 or 405 (method not allowed for GET)
```

3. **Use IP instead:**
```bash
# Get instance IP
spawn list | grep i-1234567890abcdef0

# SSH directly
ssh ec2-user@52.1.2.3
```

4. **Check instance IAM permissions:**
```bash
# On instance
aws sts get-caller-identity

# Verify DNS update permissions
curl -X POST https://dns.spore.host/register \
  -H "Content-Type: application/json" \
  -d '{"name":"test","instance_id":"'$INSTANCE_ID'"}'
```

---

#### DNSNameTaken

**Error:**
```
Error: DNS name 'my-instance' already registered
```

**Cause:** DNS name in use by another instance.

**Solutions:**

1. **Use different name:**
```bash
spawn launch --dns my-instance-2
```

2. **Check existing instances:**
```bash
spawn list | grep my-instance
```

3. **Terminate old instance:**
```bash
spawn terminate i-old-instance-id
```

4. **Wait for DNS cleanup:**
```bash
# DNS records auto-cleanup on instance termination
# Wait 5 minutes, then retry
```

---

### Resolution Failures

#### DNSNotResolving

**Error:**
```
Error: ssh: Could not resolve hostname my-instance.spore.host
```

**Cause:** DNS record not yet propagated or registration failed.

**Solutions:**

1. **Wait for propagation:**
```bash
# DNS propagation takes 1-5 minutes
sleep 300
ssh my-instance.spore.host
```

2. **Check DNS record:**
```bash
dig my-instance.spore.host
nslookup my-instance.spore.host
```

3. **Use direct IP:**
```bash
# Get IP from spawn list
spawn list | grep my-instance
ssh ec2-user@52.1.2.3
```

4. **Check registration status:**
```bash
# On instance
sudo systemctl status spored
sudo journalctl -u spored -n 50
```

---

## Job Array Issues

### State Tracking

#### SweepStateCorrupted

**Error:**
```
Error: sweep state corrupted: missing instances in DynamoDB
```

**Cause:** DynamoDB state tracking issues.

**Solutions:**

1. **Recover state:**
```bash
spawn sweep recover sweep-abc123def456
```

2. **List instances manually:**
```bash
# Query EC2 directly
aws ec2 describe-instances \
  --filters "Name=tag:SweepID,Values=sweep-abc123def456" \
  --query 'Reservations[*].Instances[*].[InstanceId,State.Name]' \
  --output table
```

3. **Resume sweep:**
```bash
spawn sweep resume sweep-abc123def456
```

4. **Create new sweep:**
```bash
# If recovery fails
spawn launch --params sweep.yaml
```

---

### Result Collection

#### CollectionFailed

**Error:**
```
Error: failed to collect results from 15/100 instances
```

**Cause:** Instances terminated early, S3 upload failures, or network issues.

**Solutions:**

1. **Retry collection:**
```bash
spawn collect sweep-abc123def456 --retry-failed
```

2. **Check instance logs:**
```bash
# Find failed instances
spawn sweep status sweep-abc123def456 | grep FAILED

# Check logs
spawn logs i-failed-instance-id
```

3. **Collect manually:**
```bash
# List successful uploads
aws s3 ls s3://spawn-results-us-east-1/sweep-abc123def456/

# Download
aws s3 sync \
  s3://spawn-results-us-east-1/sweep-abc123def456/ \
  ./results/
```

4. **Verify S3 permissions:**
```bash
# Instances need s3:PutObject
spawn launch --params sweep.yaml \
  --iam-policy s3:PutObject
```

---

## Spot Instance Issues

### Spot Interruptions

#### SpotInstanceInterrupted

**Error (on instance):**
```
Warning: Spot instance interruption in 2 minutes
```

**Cause:** AWS reclaiming spot capacity (normal behavior).

**Solutions:**

1. **Enable auto-resume:**
```bash
spawn launch --params sweep.yaml \
  --spot \
  --on-interrupt resume
```

2. **Use checkpoint/restart:**
```yaml
# sweep.yaml
base_command: |
  #!/bin/bash
  # Check for checkpoint
  if [ -f /mnt/checkpoint.dat ]; then
    ./resume_from_checkpoint /mnt/checkpoint.dat
  else
    ./start_fresh
  fi

  # Save checkpoint every 10 minutes
  trap './save_checkpoint' USR1
```

3. **Monitor interruption rate:**
```bash
spawn spot-stats --instance-type c5.large --region us-east-1
```

4. **Use on-demand for critical tasks:**
```bash
spawn launch --params sweep.yaml
# Remove --spot flag
```

---

#### SpotRequestFailed

**Error:**
```
Error: SpotMaxPriceTooLow
Your spot max price is lower than the current spot price
```

**Cause:** Spot price exceeded max price.

**Solutions:**

1. **Remove max price (use on-demand max):**
```bash
spawn launch --params sweep.yaml --spot
# Without --spot-max-price
```

2. **Increase max price:**
```bash
spawn launch --params sweep.yaml \
  --spot \
  --spot-max-price 0.15
```

3. **Check current spot prices:**
```bash
aws ec2 describe-spot-price-history \
  --instance-types c5.large \
  --start-time $(date -u +%Y-%m-%dT%H:%M:%S) \
  --product-descriptions "Linux/UNIX" \
  --query 'SpotPriceHistory[*].[AvailabilityZone,SpotPrice]' \
  --output table
```

---

## Networking Issues

### Security Group Issues

#### InvalidGroup

**Error:**
```
Error: InvalidGroup.NotFound
The security group 'sg-12345678' does not exist
```

**Cause:** Security group doesn't exist or is in different VPC.

**Solutions:**

1. **Let spawn auto-create:**
```bash
# Remove --security-group flag
spawn launch --instance-type t3.micro
```

2. **List available security groups:**
```bash
aws ec2 describe-security-groups \
  --query 'SecurityGroups[*].[GroupId,GroupName,VpcId]' \
  --output table
```

3. **Create security group:**
```bash
# Get default VPC
vpc_id=$(aws ec2 describe-vpcs \
  --filters "Name=isDefault,Values=true" \
  --query 'Vpcs[0].VpcId' \
  --output text)

# Create security group
aws ec2 create-security-group \
  --group-name spawn-sg \
  --description "spawn instances" \
  --vpc-id $vpc_id
```

---

### VPC/Subnet Issues

#### InvalidSubnetID

**Error:**
```
Error: InvalidSubnetID.NotFound
The subnet 'subnet-12345678' does not exist
```

**Cause:** Subnet doesn't exist or is in different region.

**Solutions:**

1. **Use default VPC:**
```bash
# Remove --subnet flag
spawn launch --instance-type t3.micro --region us-east-1
```

2. **List available subnets:**
```bash
aws ec2 describe-subnets \
  --filters "Name=vpc-id,Values=$VPC_ID" \
  --query 'Subnets[*].[SubnetId,AvailabilityZone,CidrBlock]' \
  --output table
```

3. **Create subnet:**
```bash
aws ec2 create-subnet \
  --vpc-id $VPC_ID \
  --cidr-block 10.0.1.0/24 \
  --availability-zone us-east-1a
```

---

## Diagnostic Commands

### Instance Diagnostics

#### Check instance status

```bash
# List all instances
spawn list

# Detailed instance info
spawn status i-1234567890abcdef0

# System log (boot messages)
aws ec2 get-console-output \
  --instance-id i-1234567890abcdef0 \
  --region us-east-1 \
  --output text
```

#### SSH to instance

```bash
# Using spawn
spawn ssh i-1234567890abcdef0

# Using AWS Session Manager (no SSH key needed)
aws ssm start-session --target i-1234567890abcdef0

# Direct SSH
ssh -i ~/.ssh/spawn-key.pem ec2-user@$(spawn list | grep i-1234567890abcdef0 | awk '{print $4}')
```

#### Check logs on instance

```bash
# Userdata execution log
sudo cat /var/log/cloud-init-output.log

# System log
sudo journalctl -xe

# Spored agent log
sudo journalctl -u spored -n 100

# Application logs
tail -f /var/log/application.log
```

---

### Sweep Diagnostics

#### Check sweep status

```bash
# Overall sweep status
spawn sweep status sweep-abc123def456

# Detailed instance status
spawn sweep status sweep-abc123def456 --verbose

# Failed instances only
spawn sweep status sweep-abc123def456 | grep FAILED
```

#### Check DynamoDB state

```bash
# Query sweep metadata
aws dynamodb get-item \
  --table-name spawn-sweeps \
  --key '{"SweepID":{"S":"sweep-abc123def456"}}'

# Query instance states
aws dynamodb query \
  --table-name spawn-sweep-instances \
  --key-condition-expression "SweepID = :sweep_id" \
  --expression-attribute-values '{":sweep_id":{"S":"sweep-abc123def456"}}'
```

---

### Network Diagnostics

#### Test connectivity

```bash
# From local machine to AWS
ping ec2.us-east-1.amazonaws.com

# Test S3 access
aws s3 ls s3://spawn-staging-us-east-1/

# Test EC2 API
aws ec2 describe-regions --region us-east-1
```

#### On instance network tests

```bash
# Check internet connectivity
curl -I https://aws.amazon.com

# Check S3 connectivity
aws s3 ls s3://spawn-staging-us-east-1/

# Check DNS resolution
dig google.com
nslookup aws.amazon.com

# Check routes
ip route show

# Check security groups
curl http://169.254.169.254/latest/meta-data/security-groups
```

---

### Cost Diagnostics

#### Check current costs

```bash
# Spawn cost tracking
spawn cost --since yesterday
spawn cost --since "7 days ago"

# AWS Cost Explorer
aws ce get-cost-and-usage \
  --time-period Start=2024-01-01,End=2024-01-31 \
  --granularity DAILY \
  --metrics BlendedCost \
  --group-by Type=DIMENSION,Key=SERVICE
```

#### Estimate future costs

```bash
# For planned sweep
spawn estimate --params sweep.yaml

# For Slurm job
spawn slurm estimate job.sbatch

# For data staging
spawn stage estimate \
  --data-size-gb 100 \
  --instances 50 \
  --regions us-east-1,us-west-2
```

---

### Quota Diagnostics

#### Check quotas

```bash
# spawn quota check
spawn quota

# AWS Service Quotas API
aws service-quotas list-service-quotas \
  --service-code ec2 \
  --query 'Quotas[?QuotaName==`Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances`]'

# Check usage
aws ec2 describe-instances \
  --filters "Name=instance-state-name,Values=running" \
  --query 'Reservations[*].Instances[*].[InstanceType]' \
  --output text | sort | uniq -c
```

---

## Getting Help

### Before Asking for Help

**Collect diagnostic information:**

```bash
# 1. Spawn version
spawn --version

# 2. List instances
spawn list > instances.txt

# 3. Recent errors
spawn logs --since 1h > errors.txt

# 4. Sweep status (if applicable)
spawn sweep status sweep-abc123def456 > sweep-status.txt

# 5. AWS configuration
aws configure list

# 6. Region info
aws ec2 describe-regions --output table
```

---

### Reporting Bugs

**Create GitHub issue with:**

1. **Clear title:**
   - ✅ "InsufficientCapacity error when launching c5.4xlarge in us-east-1"
   - ❌ "It doesn't work"

2. **Environment:**
```
spawn version: 0.9.0
OS: macOS 14.1
AWS Region: us-east-1
Instance Type: c5.4xlarge
```

3. **Steps to reproduce:**
```bash
spawn launch --instance-type c5.4xlarge --region us-east-1 --spot
```

4. **Expected behavior:**
   - "Instance should launch successfully"

5. **Actual behavior:**
```
Error: InsufficientInstanceCapacity
We currently do not have sufficient c5.4xlarge capacity...
```

6. **Logs:**
```
# Attach spawn-debug.log
spawn launch --instance-type c5.4xlarge --verbose 2>&1 | tee spawn-debug.log
```

---

### Feature Requests

**Include:**

1. **Use case:**
   - "I need to run 10,000 concurrent instances for daily ML training"

2. **Current workaround:**
   - "Currently launching in batches of 1,000, takes 2 hours"

3. **Proposed solution:**
   - "Add `--max-concurrent 10000` flag with automatic rate limiting"

4. **Priority:**
   - Critical / High / Medium / Low

---

### Getting Help

**Resources:**
- **Documentation:** https://github.com/spore-host/spore-host
- **Issues:** https://github.com/spore-host/spore-host/issues
- **Discussions:** https://github.com/spore-host/spore-host/discussions

**Response times:**
- Critical bugs: 1-2 days
- General issues: 3-7 days
- Feature requests: Best effort

---

## Summary

**Common Solutions:**
1. ✅ **Capacity errors** → Try different AZ/region or use spot
2. ✅ **Permission errors** → Check IAM policies
3. ✅ **Network errors** → Reduce parallelism, add delays
4. ✅ **Slurm issues** → Check supported directives, use #SPAWN overrides
5. ✅ **Staging issues** → Verify S3 permissions, increase timeout
6. ✅ **MPI issues** → Use auto-generated placement groups
7. ✅ **DNS issues** → Wait for propagation or use direct IP
8. ✅ **Spot interruptions** → Enable auto-resume or use checkpoints

**Debugging workflow:**
```bash
# 1. Check status
spawn list
spawn status i-instance-id

# 2. View logs
spawn logs i-instance-id
spawn ssh i-instance-id
sudo cat /var/log/cloud-init-output.log

# 3. Test manually
spawn launch --instance-type t3.micro --verbose

# 4. Report issue
# Include version, logs, and steps to reproduce
```
