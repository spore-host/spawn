# spawn create-ami

Create a custom AMI from a running or stopped instance.

## Synopsis

```bash
spawn create-ami <instance-id-or-name> [flags]
```

## Description

Create an Amazon Machine Image (AMI) from an existing instance for reusable software stacks. Useful for pre-installing software, libraries, and configurations to avoid repeating setup steps for each instance launch.

**Use Cases:**
- Pre-install ML frameworks (PyTorch, TensorFlow)
- Configure development environments
- Create GPU-enabled images with CUDA
- Snapshot production configurations

## Arguments

### instance-id-or-name
**Type:** String
**Required:** Yes
**Description:** EC2 instance ID (i-xxxxx) or instance name to create AMI from.

```bash
spawn create-ami i-0123456789abcdef0
spawn create-ami my-configured-instance
```

## Flags

### AMI Configuration

#### --name
**Type:** String
**Default:** Auto-generated with timestamp
**Description:** AMI name.

```bash
spawn create-ami i-1234567890 --name pytorch-2.2-cuda12-20260127
```

**Naming Convention:**
- Include software version: `pytorch-2.2-cuda12`
- Include date: `20260127`
- Use lowercase, hyphens (not spaces)

#### --description
**Type:** String
**Default:** Auto-generated
**Description:** AMI description.

```bash
spawn create-ami i-1234567890 \
  --name pytorch-2.2-cuda12 \
  --description "PyTorch 2.2 with CUDA 12.1 on Amazon Linux 2023"
```

#### --tags
**Type:** String (comma-separated key=value pairs)
**Default:** spawn:managed=true
**Description:** Tags for AMI.

```bash
spawn create-ami i-1234567890 \
  --name pytorch-ami \
  --tags stack=pytorch,version=2.2,cuda-version=12.1,os=al2023
```

**Recommended Tags:**
- `stack` - Software stack name (pytorch, tensorflow, custom)
- `version` - Primary software version
- `cuda-version` - CUDA version (for GPU images)
- `os` - Operating system
- `created-by` - Team or user
- `purpose` - Use case description

### Creation Options

#### --no-reboot
**Type:** Boolean
**Default:** `false`
**Description:** Create AMI without rebooting instance.

```bash
spawn create-ami i-1234567890 --no-reboot
```

**Trade-offs:**
- ✅ No downtime (instance keeps running)
- ⚠️ File system may not be consistent
- ⚠️ Best for read-only workloads

#### --wait
**Type:** Boolean
**Default:** `false`
**Description:** Wait for AMI to be available.

```bash
spawn create-ami i-1234567890 --wait
```

**Duration:** 5-15 minutes typically

### Output

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output in JSON format.

```bash
spawn create-ami i-1234567890 --json
```

## Output

### Standard Output

```
Creating AMI from instance: i-0123456789abcdef0

Instance Details:
  Name: pytorch-builder
  Type: g5.xlarge
  State: running
  Platform: Amazon Linux 2023

AMI Configuration:
  Name: pytorch-2.2-cuda12-20260127
  Description: PyTorch 2.2 with CUDA 12.1 on Amazon Linux 2023
  Tags: stack=pytorch, version=2.2, cuda-version=12.1

Creating AMI...
  ✓ Creating snapshot of root volume (100 GB)
  ✓ Registering AMI
  ✓ Applying tags

AMI created successfully!

AMI ID: ami-0abc123def456789
Name: pytorch-2.2-cuda12-20260127
State: pending → available (this takes 5-15 minutes)

Usage:
  spawn launch --instance-type g5.xlarge --ami ami-0abc123def456789

  # Or by name (if unique)
  spawn launch --instance-type g5.xlarge --ami pytorch-2.2-cuda12-20260127

List AMIs:
  spawn list-amis --stack pytorch
```

### JSON Output

```json
{
  "ami_id": "ami-0abc123def456789",
  "name": "pytorch-2.2-cuda12-20260127",
  "description": "PyTorch 2.2 with CUDA 12.1 on Amazon Linux 2023",
  "state": "pending",
  "source_instance_id": "i-0123456789abcdef0",
  "source_instance_name": "pytorch-builder",
  "region": "us-east-1",
  "architecture": "x86_64",
  "root_device_type": "ebs",
  "virtualization_type": "hvm",
  "created_at": "2026-01-27T15:30:00Z",
  "tags": {
    "Name": "pytorch-2.2-cuda12-20260127",
    "spawn:managed": "true",
    "stack": "pytorch",
    "version": "2.2",
    "cuda-version": "12.1"
  }
}
```

## Examples

### Create PyTorch AMI
```bash
# Step 1: Launch instance and install software
spawn launch --instance-type g5.xlarge --name pytorch-builder --ttl 2h
spawn connect pytorch-builder

# On instance:
pip3 install torch torchvision torchaudio --index-url https://download.pytorch.org/whl/cu121
pip3 install transformers accelerate datasets
# ... more installation ...
exit

# Step 2: Create AMI
spawn create-ami pytorch-builder \
  --name pytorch-2.2-cuda12-$(date +%Y%m%d) \
  --description "PyTorch 2.2 with CUDA 12.1" \
  --tags stack=pytorch,version=2.2,cuda-version=12.1 \
  --wait

# Step 3: Terminate builder instance
spawn cancel pytorch-builder
```

### Create TensorFlow AMI
```bash
spawn launch --instance-type g5.xlarge --name tf-builder
spawn connect tf-builder
# Install TensorFlow...
exit

spawn create-ami tf-builder \
  --name tensorflow-2.15-cuda12-$(date +%Y%m%d) \
  --tags stack=tensorflow,version=2.15,cuda-version=12.1
```

### Create Custom Development Environment
```bash
spawn launch --instance-type t3.large --name dev-builder
spawn connect dev-builder
# Install tools: docker, kubectl, awscli, etc.
exit

spawn create-ami dev-builder \
  --name devenv-$(date +%Y%m%d) \
  --description "Development environment with Docker, kubectl, AWS CLI" \
  --tags stack=devenv,tools=docker-kubectl-aws
```

### Create Without Rebooting
```bash
# Instance keeps running during AMI creation
spawn create-ami i-1234567890 \
  --name my-ami \
  --no-reboot \
  --wait
```

### Automation: Build and Deploy AMI
```bash
#!/bin/bash
# build-ami.sh

INSTANCE_TYPE="g5.xlarge"
AMI_NAME="pytorch-$(date +%Y%m%d-%H%M)"

# Launch builder instance
echo "Launching builder instance..."
INSTANCE=$(spawn launch --instance-type "$INSTANCE_TYPE" --name builder --quiet)

# Wait for SSH
spawn connect "$INSTANCE" --wait

# Install software
echo "Installing PyTorch..."
spawn connect "$INSTANCE" -c "pip3 install torch torchvision torchaudio"

# Create AMI
echo "Creating AMI..."
AMI_ID=$(spawn create-ami "$INSTANCE" --name "$AMI_NAME" --wait --json | jq -r '.ami_id')

# Terminate builder
echo "Terminating builder..."
aws ec2 terminate-instances --instance-ids "$INSTANCE"

echo "AMI created: $AMI_ID"
echo "Launch with: spawn launch --instance-type $INSTANCE_TYPE --ami $AMI_ID"
```

## AMI Management Best Practices

### 1. Use Descriptive Names
```bash
# Good
pytorch-2.2-cuda12-20260127
tensorflow-2.15-cuda11-al2023-20260127

# Bad
my-ami
test
image-1
```

### 2. Tag Comprehensively
```bash
spawn create-ami builder \
  --name pytorch-2.2-cuda12 \
  --tags "
    stack=pytorch,
    version=2.2,
    cuda-version=12.1,
    os=al2023,
    created-by=ml-team,
    purpose=training,
    expires=2026-06-01
  "
```

### 3. Document in Description
```bash
spawn create-ami builder \
  --description "PyTorch 2.2.0 + CUDA 12.1 + cuDNN 8.9 on AL2023. Includes: transformers, accelerate, datasets, wandb. NVIDIA driver 535.104.05"
```

### 4. Regular Rebuilds
- Rebuild monthly to get security updates
- Use date in AMI name for versioning
- Deregister old AMIs after validation

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | AMI created successfully |
| 1 | AMI creation failed (EC2 error, snapshot failure) |
| 2 | Invalid arguments (missing instance ID, invalid tags) |
| 3 | Instance not found (instance doesn't exist) |
| 4 | Instance state invalid (must be running or stopped) |

## Troubleshooting

### "Instance must be stopped"
```bash
# For no-reboot=false, instance must be stopped
spawn connect i-1234567890 -c "sudo shutdown -h now"

# Wait for stopped state
aws ec2 wait instance-stopped --instance-ids i-1234567890

# Then create AMI
spawn create-ami i-1234567890
```

### AMI Creation Takes Too Long
```bash
# Large root volumes take longer
# 100 GB: ~10 minutes
# 500 GB: ~30 minutes

# Check progress
aws ec2 describe-images --image-ids ami-xxx --query 'Images[0].State'
```

### "Insufficient permissions"
```bash
# Required IAM permissions:
# - ec2:CreateImage
# - ec2:CreateSnapshot
# - ec2:CreateTags
# - ec2:DescribeImages
# - ec2:DescribeInstances
```

## Performance

- **Snapshot creation:** ~1 minute per 10 GB (varies)
- **AMI registration:** ~30 seconds
- **Availability:** 5-15 minutes total

## Cost

- **Snapshot storage:** $0.05/GB/month (EBS snapshot pricing)
- **No cost for AMI itself** (only underlying snapshots)

**Example:** 100 GB AMI = $5/month storage cost

## See Also
- [spawn list-amis](list-amis.md) - List spawn-managed AMIs
- [spawn launch](launch.md) - Launch from custom AMI
- [AMI Management Guide](../../how-to/ami-management.md) - Complete AMI workflow
