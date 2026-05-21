# How-To: Custom AMIs

Create and manage custom Amazon Machine Images (AMIs) for faster instance launches.

## When to Use Custom AMIs

### Use Custom AMIs When:

- **Pre-installed software** - Install once, use many times
- **Faster launches** - No wait for package installation
- **Consistent environments** - Same configuration across all instances
- **Large dependencies** - Don't download PyTorch/CUDA every time

### Stick with Default AMIs When:

- **Quick one-off tasks** - Not worth building AMI
- **Frequently changing dependencies** - AMI becomes outdated
- **Testing different configurations** - More flexible with user data

---

## Creating Your First Custom AMI

### Step 1: Launch Base Instance

```bash
spawn launch --instance-type t3.medium --name ami-builder --ttl 2h
```

### Step 2: Install Software

```bash
# Connect to instance
spawn connect ami-builder

# Update system
sudo yum update -y

# Install common tools
sudo yum install -y git tmux htop vim

# Install Python packages
pip3 install --user torch torchvision numpy pandas matplotlib scikit-learn

# Install CUDA tools (for GPU AMI)
# Already installed on Amazon Linux 2023 GPU AMIs

# Clean up
sudo yum clean all
rm -rf ~/.cache/pip

exit
```

### Step 3: Create AMI

```bash
# Get instance ID
INSTANCE_ID=$(spawn list --tag Name=ami-builder --format json | jq -r '.[0].instance_id')

# Create AMI
spawn create-ami $INSTANCE_ID --name my-ml-ami --description "ML environment with PyTorch"
```

**Output:**
```
Creating AMI from i-0abc123def456789...

✓ Stopping instance
✓ Creating AMI: my-ml-ami
✓ Waiting for AMI to be available...

AMI created: ami-0xyz789abc123def4

Launch with:
  spawn launch --ami ami-0xyz789abc123def4

Elapsed: 5m 32s
```

### Step 4: Test AMI

```bash
# Launch instance with custom AMI
spawn launch --instance-type t3.medium --ami ami-0xyz789abc123def4 --name test-custom-ami

# Connect and verify
spawn connect test-custom-ami

# Check installed packages
python3 -c "import torch; print(torch.__version__)"
# Should show installed PyTorch version
```

---

## ML/Data Science AMI

### Complete ML Environment

```bash
#!/bin/bash
# setup-ml-ami.sh - Run on base instance

set -e

echo "=== Installing ML Environment ==="

# System packages
sudo yum update -y
sudo yum install -y \
  git vim tmux htop \
  gcc gcc-c++ make \
  openssl-devel bzip2-devel libffi-devel

# Python packages
pip3 install --user \
  torch torchvision torchaudio --index-url https://download.pytorch.org/whl/cu118 \
  transformers datasets accelerate \
  numpy pandas scikit-learn matplotlib seaborn \
  jupyter jupyterlab \
  wandb tensorboard \
  black pylint pytest

# Jupyter extensions
pip3 install --user jupyterlab-git

# AWS tools
pip3 install --user boto3 awscli

# Clean up
sudo yum clean all
rm -rf ~/.cache/pip
rm -rf /tmp/*

echo "=== ML Environment Setup Complete ==="
```

**Build AMI:**
```bash
# Launch builder
spawn launch --instance-type g5.xlarge --name ml-ami-builder --ttl 2h

# Upload script
scp setup-ml-ami.sh ec2-user@<instance-ip>:~/

# Run setup
spawn connect ml-ami-builder -c "bash ~/setup-ml-ami.sh"

# Create AMI
INSTANCE_ID=$(spawn list --tag Name=ml-ami-builder --quiet)
spawn create-ami $INSTANCE_ID --name ml-gpu-ami --description "ML environment with PyTorch, Transformers, Jupyter"
```

---

## Web Server AMI

### NGINX + Node.js Environment

```bash
#!/bin/bash
# setup-web-ami.sh

set -e

echo "=== Installing Web Server Environment ==="

# Install Node.js 20.x
curl -fsSL https://rpm.nodesource.com/setup_20.x | sudo bash -
sudo yum install -y nodejs

# Install NGINX
sudo yum install -y nginx

# Install PM2 (process manager)
sudo npm install -g pm2

# Install common Node packages globally
sudo npm install -g yarn typescript eslint prettier

# Configure NGINX
sudo systemctl enable nginx

# Configure PM2 to start on boot
sudo pm2 startup systemd -u ec2-user --hp /home/ec2-user
sudo pm2 save

# Clean up
sudo yum clean all
sudo npm cache clean --force

echo "=== Web Server Environment Setup Complete ==="
```

---

## Docker-Based AMI

### Docker + Docker Compose

```bash
#!/bin/bash
# setup-docker-ami.sh

set -e

echo "=== Installing Docker Environment ==="

# Install Docker
sudo yum install -y docker

# Start Docker service
sudo systemctl start docker
sudo systemctl enable docker

# Add ec2-user to docker group
sudo usermod -aG docker ec2-user

# Install Docker Compose
sudo curl -L "https://github.com/docker/compose/releases/latest/download/docker-compose-$(uname -s)-$(uname -m)" \
  -o /usr/local/bin/docker-compose
sudo chmod +x /usr/local/bin/docker-compose

# Verify installations
docker --version
docker-compose --version

echo "=== Docker Environment Setup Complete ==="
echo "Log out and back in for docker group changes to take effect"
```

---

## AMI Best Practices

### 1. Clean Up Before Creating AMI

```bash
# Remove logs
sudo rm -rf /var/log/*
sudo find /var/log -type f -exec truncate -s 0 {} \;

# Remove bash history
rm ~/.bash_history
history -c

# Remove SSH keys (will be injected at launch)
rm -f ~/.ssh/authorized_keys

# Remove cloud-init artifacts
sudo rm -rf /var/lib/cloud/instances/*

# Remove temporary files
rm -rf /tmp/*
sudo rm -rf /tmp/*

# Clear package caches
sudo yum clean all
pip3 cache purge
```

### 2. Generalize Configuration

```bash
# Don't hardcode instance-specific values
# Good:
aws s3 cp s3://my-bucket/data/ /data/

# Bad:
aws s3 cp s3://my-bucket/data-i-0abc123/ /data/  # Hardcoded instance ID
```

### 3. Document Installed Software

```bash
# Create manifest file
cat > /etc/ami-manifest.txt << EOF
AMI Name: ml-gpu-ami
Build Date: $(date -u +%Y-%m-%d)
Base AMI: ami-0abc123def (Amazon Linux 2023)

Installed Packages:
- Python $(python3 --version 2>&1 | cut -d' ' -f2)
- PyTorch $(python3 -c 'import torch; print(torch.__version__)' 2>/dev/null || echo 'N/A')
- CUDA $(nvcc --version 2>/dev/null | grep release | cut -d' ' -f5 | tr -d ',')

Custom Configuration:
- Jupyter Lab installed
- PyTorch pre-installed
- AWS CLI configured
EOF
```

### 4. Version Your AMIs

```bash
# Use semantic versioning
spawn create-ami $INSTANCE_ID --name ml-ami-v1.0.0
spawn create-ami $INSTANCE_ID --name ml-ami-v1.1.0
spawn create-ami $INSTANCE_ID --name ml-ami-v2.0.0
```

---

## AMI Management

### List Your AMIs

```bash
spawn list-amis

# Filter by name
spawn list-amis --name "ml-ami-*"

# Filter by tag
spawn list-amis --tag project=ml
```

### Tag AMIs

```bash
# Tag during creation
spawn create-ami $INSTANCE_ID \
  --name my-ami \
  --tags project=ml,version=1.0,owner=alice

# Tag existing AMI
aws ec2 create-tags \
  --resources ami-0xyz789abc123def4 \
  --tags Key=project,Value=ml Key=version,Value=1.0
```

### Share AMIs

**Share with specific AWS account:**
```bash
AMI_ID="ami-0xyz789abc123def4"
ACCOUNT_ID="123456789012"

aws ec2 modify-image-attribute \
  --image-id $AMI_ID \
  --launch-permission "Add=[{UserId=$ACCOUNT_ID}]"
```

**Make AMI public (careful!):**
```bash
aws ec2 modify-image-attribute \
  --image-id $AMI_ID \
  --launch-permission "Add=[{Group=all}]"
```

### Deregister Old AMIs

```bash
#!/bin/bash
# cleanup-old-amis.sh

# List AMIs older than 90 days
OLD_AMIS=$(aws ec2 describe-images \
  --owners self \
  --query "Images[?CreationDate<='$(date -u -d '90 days ago' +%Y-%m-%d)'].[ImageId,Name,CreationDate]" \
  --output text)

echo "$OLD_AMIS"

# Confirm before deleting
read -p "Delete these AMIs? (y/n) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
  echo "$OLD_AMIS" | while read ami name date; do
    echo "Deleting $ami ($name, $date)..."

    # Deregister AMI
    aws ec2 deregister-image --image-id $ami

    # Delete associated snapshots
    SNAPSHOTS=$(aws ec2 describe-snapshots \
      --owner-ids self \
      --filters "Name=description,Values=*$ami*" \
      --query 'Snapshots[*].SnapshotId' \
      --output text)

    for snapshot in $SNAPSHOTS; do
      echo "Deleting snapshot $snapshot..."
      aws ec2 delete-snapshot --snapshot-id $snapshot
    done
  done
fi
```

---

## Multi-Region AMIs

### Copy AMI to Another Region

```bash
SOURCE_AMI="ami-0xyz789abc123def4"
SOURCE_REGION="us-east-1"
TARGET_REGION="us-west-2"

# Copy AMI
TARGET_AMI=$(aws ec2 copy-image \
  --source-region $SOURCE_REGION \
  --source-image-id $SOURCE_AMI \
  --region $TARGET_REGION \
  --name "my-ml-ami" \
  --description "Copy of $SOURCE_AMI from $SOURCE_REGION" \
  --query 'ImageId' \
  --output text)

echo "Copied to $TARGET_REGION: $TARGET_AMI"

# Wait for copy to complete
aws ec2 wait image-available --region $TARGET_REGION --image-ids $TARGET_AMI

echo "Copy complete!"
```

### Automated Multi-Region Distribution

```bash
#!/bin/bash
# distribute-ami.sh

SOURCE_AMI="ami-0xyz789abc123def4"
SOURCE_REGION="us-east-1"
TARGET_REGIONS="us-west-2 eu-west-1 ap-southeast-1"

for region in $TARGET_REGIONS; do
  echo "Copying to $region..."

  TARGET_AMI=$(aws ec2 copy-image \
    --source-region $SOURCE_REGION \
    --source-image-id $SOURCE_AMI \
    --region $region \
    --name "my-ml-ami" \
    --query 'ImageId' \
    --output text)

  echo "$region: $TARGET_AMI"

  # Tag in target region
  aws ec2 create-tags \
    --region $region \
    --resources $TARGET_AMI \
    --tags Key=source-ami,Value=$SOURCE_AMI Key=source-region,Value=$SOURCE_REGION
done

echo "Distribution complete!"
```

---

## Golden Image Pipeline

### Automated AMI Building

```bash
#!/bin/bash
# build-golden-image.sh

set -e

echo "=== Building Golden Image ==="

# 1. Launch builder instance
INSTANCE_ID=$(spawn launch \
  --instance-type t3.medium \
  --name ami-builder \
  --ttl 2h \
  --wait-for-ssh \
  --quiet)

echo "Builder launched: $INSTANCE_ID"

# 2. Upload setup script
scp setup-ami.sh ec2-user@$(spawn status $INSTANCE_ID --json | jq -r '.network.public_ip'):~/

# 3. Run setup
spawn connect $INSTANCE_ID -c "bash ~/setup-ami.sh"

# 4. Create AMI
AMI_ID=$(spawn create-ami $INSTANCE_ID \
  --name "golden-image-$(date +%Y%m%d-%H%M%S)" \
  --tags project=golden-image,build-date=$(date +%Y%m%d) \
  --quiet)

echo "AMI created: $AMI_ID"

# 5. Test AMI
echo "Testing AMI..."
TEST_INSTANCE=$(spawn launch \
  --ami $AMI_ID \
  --instance-type t3.micro \
  --ttl 30m \
  --wait-for-ssh \
  --quiet)

# Run smoke tests
spawn connect $TEST_INSTANCE -c "python3 -c 'import torch; print(\"OK\")'"

# 6. Terminate test instance
aws ec2 terminate-instances --instance-ids $TEST_INSTANCE

# 7. Tag as production-ready
aws ec2 create-tags \
  --resources $AMI_ID \
  --tags Key=status,Value=production-ready

echo "Golden image build complete: $AMI_ID"
```

**Run in CI/CD:**
```yaml
# .github/workflows/build-ami.yml
name: Build Golden Image

on:
  schedule:
    - cron: '0 0 * * 0'  # Weekly on Sunday
  workflow_dispatch:

jobs:
  build-ami:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - name: Configure AWS
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1
      - name: Build AMI
        run: ./build-golden-image.sh
```

---

## AMI Costs

### Storage Costs

**EBS Snapshot Storage:**
- $0.05/GB/month (standard snapshots)

**Example:**
- 50 GB AMI = $2.50/month

**Cost optimization:**
- Delete old/unused AMIs
- Use smaller base images
- Clean up before creating AMI

---

## Troubleshooting

### AMI Creation Hangs

**Problem:** AMI creation stuck at "pending" for > 30 minutes.

**Cause:** Instance has large volumes or I/O-heavy workloads.

**Solution:**
```bash
# Stop all I/O before creating AMI
spawn connect $INSTANCE_ID -c "sudo sync && sudo systemctl stop myapp"

# Then create AMI
spawn create-ami $INSTANCE_ID --name my-ami
```

### Instance Won't Launch from AMI

**Problem:** Instance fails to boot or SSH times out.

**Causes:**
1. Corrupted AMI
2. Missing essential files
3. Wrong permissions

**Solution:**
```bash
# Launch in same region as AMI
spawn launch --ami $AMI_ID --region us-east-1

# Check system logs
aws ec2 get-console-output --instance-id $INSTANCE_ID

# Rebuild AMI from scratch if corrupted
```

---

## See Also

- [spawn create-ami](../reference/commands/create-ami.md) - Create AMI command
- [spawn list-amis](../reference/commands/list-amis.md) - List AMIs command
- [Tutorial 2: Your First Instance](../tutorials/02-first-instance.md) - AMI basics
- [AWS AMI Documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/AMIs.html)
