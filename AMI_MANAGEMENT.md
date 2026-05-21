# AMI Management for Spawn

## Overview

Custom AMIs enable reusable software stacks, eliminating the need to install dependencies via user-data on every launch. This document outlines a comprehensive AMI lifecycle management system for spawn.

## Current State

Spawn already supports custom AMIs:

```bash
# Auto-detect AL2023 AMI (current default)
spawn launch --instance-type m7i.large

# Use custom AMI
spawn launch --instance-type m7i.large --ami ami-0123456789abcdef0
```

**Auto-detection logic:**
- Detects architecture (x86_64 vs ARM/Graviton)
- Detects GPU requirements
- Selects appropriate AL2023 AMI (base or GPU-enabled)

## Motivation: Why Custom AMIs?

### ✅ Use AMIs When:
- **Software stack is stable** - Not changing frequently
- **Installation is slow** - >5 minutes to install dependencies
- **Reproducibility is critical** - Exact versions, tested configurations
- **Launching frequently** - Amortize creation cost over many launches
- **Complex dependencies** - System libraries, kernel modules, CUDA

### ❌ Use user-data When:
- **Software changes frequently** - Still iterating
- **Configuration varies** - Different configs per launch
- **Installation is fast** - <2 minutes
- **One-off experiments** - Not worth AMI overhead

### Example: PyTorch Deep Learning Stack

**Without AMI (slow):**
```bash
spawn launch --instance-type g5.xlarge --user-data @install-pytorch.sh
# 15+ minutes: Update packages, install CUDA, install PyTorch, etc.
```

**With AMI (fast):**
```bash
spawn launch --instance-type g5.xlarge --ami ami-pytorch-cuda12
# 30 seconds: Everything pre-installed, ready to train
```

---

## AMI Lifecycle Stages

```
1. PREPARE     → Launch instance, install software
2. CREATE      → Create AMI from instance
3. TAG         → Add metadata tags
4. TEST        → Validate AMI works
5. PUBLISH     → Share/replicate if needed
6. USE         → Launch instances from AMI
7. DEPRECATE   → Mark old versions as deprecated
8. CLEANUP     → Deregister unused AMIs
```

---

## 1. Creating AMIs

### Approach A: Manual (Current)

**Step 1: Launch and prepare instance**
```bash
# Launch base instance
spawn launch --instance-type m7i.large --name ami-builder --ttl 2h

# Connect and install software
spawn connect ami-builder

# Install your stack
sudo yum update -y
sudo yum install -y python3.11 python3.11-pip git
pip3.11 install torch torchvision torchaudio --index-url https://download.pytorch.org/whl/cpu
# ... more installation steps ...

# Clean up before AMI creation
sudo yum clean all
rm -rf ~/.bash_history
sudo rm -rf /tmp/*
exit
```

**Step 2: Create AMI via AWS CLI**
```bash
# Get instance ID
INSTANCE_ID=$(spawn list --name ami-builder -o json | jq -r '.[0].instance_id')

# Create AMI
aws ec2 create-image \
  --instance-id $INSTANCE_ID \
  --name "pytorch-2.2-cpu-$(date +%Y%m%d)" \
  --description "PyTorch 2.2 CPU on AL2023" \
  --tag-specifications 'ResourceType=image,Tags=[
    {Key=spawn:stack,Value=pytorch},
    {Key=spawn:version,Value=2.2},
    {Key=spawn:arch,Value=x86_64},
    {Key=spawn:created,Value='$(date -u +%Y-%m-%dT%H:%M:%SZ)'},
    {Key=spawn:base,Value=al2023}
  ]' \
  --no-reboot  # Keep instance running for testing

# Wait for AMI to be available
aws ec2 wait image-available --image-ids ami-xxxxx

# Terminate builder instance
spawn terminate ami-builder
```

### Approach B: Automated (Proposed)

**New command: `spawn create-ami`**

```bash
# Create AMI from running instance
spawn create-ami <instance-id-or-name> \
  --name pytorch-2.2-cpu \
  --description "PyTorch 2.2 CPU on AL2023" \
  --tag stack=pytorch \
  --tag version=2.2 \
  --reboot  # Default: --no-reboot

# With automatic cleanup script first
spawn create-ami ami-builder \
  --name my-stack \
  --cleanup-script @cleanup.sh \
  --wait  # Wait for AMI to be available
```

**Cleanup script example:**
```bash
#!/bin/bash
# cleanup.sh - Run before AMI creation

# Remove temp files
sudo rm -rf /tmp/*
sudo rm -rf /var/tmp/*

# Clear logs
sudo truncate -s 0 /var/log/*.log

# Clear bash history
rm -f ~/.bash_history
history -c

# Remove SSH host keys (regenerated on first boot)
sudo rm -f /etc/ssh/ssh_host_*

# Clear package cache
sudo yum clean all

# Clear cloud-init state (allows re-run on new instances)
sudo cloud-init clean --logs
```

---

## 2. AMI Health Checks

spawn automatically tracks AMI health and warns about outdated AMIs.

### Base AMI Tracking

When you create an AMI, spawn automatically:
1. **Records the base AMI ID** - The AMI that the source instance was launched from
2. **Stores it in tags** - `spawn:base-ami` tag on your custom AMI

### How the Health Check Works

When you run `spawn list-amis`, spawn:
1. **Reads your base AMI** - From the `spawn:base-ami` tag
2. **Gets current recommended AMI** - Queries AWS for latest AL2023 AMI
3. **Compares them** - If different, a newer base AMI is available
4. **Checks age** - Calculates how old your base AMI is
5. **Generates warnings** - Based on severity

**Key insight**: spawn checks if AWS has released a NEWER base AMI, not just if yours is old. If your AMI uses the current base, no warning appears even if it's 60 days old.

### Health Check Display

```bash
$ spawn list-amis

NAME                  AMI ID                 STACK    VERSION  ARCH    SIZE  AGE   STATUS
pytorch-old           ami-abc123             pytorch  2.1      x86_64  30GB  95d   GPU ⚠️
pytorch-medium        ami-def456             pytorch  2.2      x86_64  30GB  45d   GPU ⚠️
pytorch-current       ami-ghi789             pytorch  2.3      x86_64  30GB  5d    GPU

Warnings:
  pytorch-old:
    - newer base AMI available (current: ami-xyz789, yours: ami-old123, age: 95d) - rebuild recommended
  pytorch-medium:
    - newer base AMI available (current: ami-xyz789, yours: ami-old456, age: 45d)
```

**What the warning shows:**
- **Current**: The latest recommended AL2023 AMI
- **Yours**: The base AMI your custom AMI was built from
- **Age**: How old your base AMI is
- **Action**: "rebuild recommended" for critical (>90 days)

### Warning Levels

- **30-90 days**: Warning - base AMI is aging, consider rebuild
- **>90 days**: Critical - base AMI is very old, security patches missing

### Automatic spored Updates

**Good news**: The spored agent auto-updates on every launch!

spawn's user-data script downloads the latest spored binary from S3 during instance startup. This means:
- ✅ AMIs always boot with the latest spored agent
- ✅ No need to rebuild AMIs for spored updates
- ✅ Security fixes and features automatically deployed

**Only base AMI changes require rebuilding the AMI.**

### When to Rebuild

Rebuild your AMI when:
1. **Base AMI is >90 days old** - Security patches and kernel updates
2. **Software stack updates** - New PyTorch version, library updates, etc.
3. **Configuration changes** - System-level settings, kernel parameters
4. **Security vulnerabilities** - CVEs in installed packages

**Don't rebuild for:**
- spored agent updates (auto-updated on launch)
- Environment variable changes (use user-data)
- Temporary configuration (use user-data)

## 3. Tagging Conventions

### Automatic Tags (Added by spawn)

```
spawn:managed       = "true"                          # spawn-managed AMI
spawn:created       = "2026-01-14T10:30:00Z"          # ISO 8601 timestamp
spawn:created-from  = "i-abc123"                      # Source instance ID
spawn:source-region = "us-east-1"                     # Region where created
spawn:arch          = "x86_64" | "arm64"              # CPU architecture
spawn:gpu           = "true" | "false"                # GPU support
spawn:base-ami      = "ami-xyz789"                    # Base AMI used (for health checks)
```

### Recommended User Tags

```
stack               = "pytorch" | "tensorflow" | "mpi" | "genomics"
version             = "2.2" | "1.0.0" | semver
base                = "al2023" | "ubuntu22" | "rhel9"
cuda-version        = "12.1" | "11.8" (if GPU)
owner               = "team-ml" | "john.doe@company.com"
env                 = "dev" | "staging" | "prod"
```

### Lifecycle Tags (Added later)

```
spawn:deprecated    = "true" (when deprecating)
```

**Optional tags:**
```
spawn:python        = "3.11"
spawn:node          = "20"
spawn:framework     = "pytorch" | "jax" | "tensorflow"
spawn:size          = "30GB" (AMI size for cost tracking)
```

### Why Consistent Tags?

1. **Discovery**: Find AMIs by stack, version, architecture
2. **Automation**: Scripts can select appropriate AMI
3. **Lifecycle**: Track creation date, deprecate old versions
4. **Cost tracking**: Identify AMI storage costs

---

## 3. AMI Discovery

### Current: Manual Lookup

```bash
# List AMIs manually
aws ec2 describe-images \
  --owners self \
  --filters "Name=tag:spawn:stack,Values=pytorch"
```

### Proposed: `spawn list-amis`

```bash
# List all spawn-managed AMIs
spawn list-amis

# Filter by stack
spawn list-amis --stack pytorch

# Filter by version
spawn list-amis --stack pytorch --version 2.2

# Filter by architecture
spawn list-amis --arch arm64

# Show deprecated AMIs
spawn list-amis --deprecated

# JSON output
spawn list-amis --json
```

**Example output:**
```
AMIs (4 found):

pytorch-2.2-gpu-20260114  ami-abc123  x86_64  CUDA 12.1  30GB  14 days old
pytorch-2.1-gpu-20260101  ami-def456  x86_64  CUDA 12.1  28GB  27 days old  [deprecated]
pytorch-2.2-cpu-20260114  ami-ghi789  x86_64  -          25GB  14 days old
pytorch-2.2-arm-20260114  ami-jkl012  arm64   -          23GB  14 days old
```

### Proposed: Smart AMI Selection

```bash
# Launch with AMI pattern matching
spawn launch --instance-type g5.xlarge --ami-stack pytorch --ami-version 2.2

# Automatically selects:
# - pytorch stack
# - version 2.2
# - GPU-enabled AMI (g5 = GPU instance)
# - x86_64 architecture (g5 = x86_64)
# - Latest (most recent creation date)
# - Not deprecated
```

---

## 4. AMI Versioning

### Semantic Versioning

Use semver for AMI versions: `MAJOR.MINOR.PATCH`

**Version bumps:**
- **MAJOR**: Breaking changes (Python 3.10 → 3.11, CUDA 11 → 12)
- **MINOR**: New features (added new library, upgraded framework)
- **PATCH**: Bug fixes (security patches, config tweaks)

**Example naming:**
```
pytorch-2.2.0-cuda12-x86_64-20260114
pytorch-2.1.5-cuda12-x86_64-20260101  [deprecated]
pytorch-2.1.4-cuda12-x86_64-20251220  [deprecated]
```

### Deprecation Strategy

**When to deprecate:**
- Security vulnerabilities in base OS or dependencies
- New major version released
- AMI has been superseded
- After 90 days (configurable threshold)

**Deprecation process:**
```bash
# Mark AMI as deprecated
spawn deprecate-ami ami-abc123 \
  --reason "Superseded by pytorch-2.3.0" \
  --replacement ami-xyz789

# Adds tags:
#   spawn:deprecated = "true"
#   spawn:deprecated-date = "2026-01-14T10:30:00Z"
#   spawn:replacement = "ami-xyz789"
#   spawn:deprecation-reason = "Superseded by pytorch-2.3.0"
```

**Grace period:**
- Deprecated AMIs still work but show warnings
- After 30 days, warn on every launch
- After 90 days, consider deregistering

---

## 5. AMI Cleanup

### Storage Costs

AMIs are stored as EBS snapshots:
- **Cost**: $0.05/GB/month (standard)
- **30GB AMI**: ~$1.50/month
- **10 old AMIs**: ~$15/month wasted

### Cleanup Strategy

**Automatic cleanup rules:**
1. Deregister AMIs older than 180 days (if deprecated)
2. Keep at least 2 versions of each stack
3. Delete snapshots after deregistering AMI
4. Never delete AMIs tagged as `spawn:keep=true`

**Proposed: `spawn cleanup-amis`**

```bash
# Dry run (show what would be deleted)
spawn cleanup-amis --dry-run

# Delete deprecated AMIs older than 90 days
spawn cleanup-amis --age 90d --deprecated-only

# Delete by stack
spawn cleanup-amis --stack pytorch --keep-latest 3

# Delete specific AMI
spawn cleanup-amis ami-abc123
```

**Safety checks:**
- Confirm before deletion (unless --yes flag)
- Check if AMI is in use by running instances
- Preserve AMIs with `spawn:keep=true` tag

---

## 6. AMI Sharing

### Use Cases

1. **Cross-account**: Share AMI with other AWS accounts (dev/staging/prod)
2. **Cross-team**: Share within organization
3. **Public**: Share with community (research, open source)

### Sharing Commands

**Proposed: `spawn share-ami`**

```bash
# Share with specific account
spawn share-ami ami-abc123 --account 123456789012

# Share with multiple accounts
spawn share-ami ami-abc123 \
  --account 123456789012 \
  --account 234567890123

# Make public (anyone can use)
spawn share-ami ami-abc123 --public

# Revoke sharing
spawn share-ami ami-abc123 --account 123456789012 --revoke
```

### Permissions

AMI sharing doesn't copy the AMI - recipients use the original:
- ✅ Recipient can launch instances
- ✅ Recipient can create copies
- ❌ Recipient cannot modify original
- ❌ Recipient pays for instances, not AMI storage

---

## 7. AMI Replication

### Cross-Region Deployment

**Why replicate:**
- **Lower latency**: AMI in same region as instances
- **Disaster recovery**: Copy to backup region
- **Global deployment**: Deploy to multiple regions

**Proposed: `spawn replicate-ami`**

```bash
# Copy AMI to another region
spawn replicate-ami ami-abc123 --to-region eu-west-1

# Copy to multiple regions
spawn replicate-ami ami-abc123 \
  --to-region eu-west-1 \
  --to-region ap-southeast-1 \
  --to-region us-west-2

# Copy all AMIs for a stack
spawn replicate-ami --stack pytorch --to-region eu-west-1
```

**Tagging replicas:**
- Copy all tags from source
- Add `spawn:replicated-from = "ami-abc123"`
- Add `spawn:source-region = "us-east-1"`

---

## 8. AMI Testing

### Validation Workflow

Before using AMI in production, validate it works:

**Proposed: `spawn test-ami`**

```bash
# Launch test instance from AMI
spawn test-ami ami-abc123 \
  --instance-type m7i.large \
  --test-script @validate.sh \
  --auto-terminate

# validate.sh
#!/bin/bash
set -e

# Test 1: Check Python version
python3 --version | grep "3.11"

# Test 2: Import libraries
python3 -c "import torch; print(torch.__version__)"

# Test 3: Run smoke tests
pytest /opt/tests/

echo "All tests passed!"
```

**Auto-terminate:**
- If test script succeeds (exit 0): terminate instance
- If test script fails (exit non-zero): keep instance for debugging

---

## 9. AMI Building Pipelines

### CI/CD Integration

**Automated AMI builds on code changes:**

```yaml
# .github/workflows/build-ami.yml
name: Build AMI
on:
  push:
    branches: [main]
    paths:
      - 'ami-builder/**'

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Configure AWS
        uses: aws-actions/configure-aws-credentials@v2
        with:
          role-to-assume: arn:aws:iam::ACCOUNT:role/ami-builder

      - name: Build AMI
        run: |
          # Launch builder instance
          INSTANCE=$(spawn launch \
            --instance-type m7i.large \
            --name ami-builder-${{ github.run_id }} \
            --user-data @ami-builder/install.sh \
            --ttl 1h \
            --output json | jq -r '.instance_id')

          # Wait for user-data to complete
          spawn wait $INSTANCE --for-tag spawn:build-complete=true

          # Create AMI
          AMI=$(spawn create-ami $INSTANCE \
            --name "pytorch-2.2-$(date +%Y%m%d)-${{ github.sha }}" \
            --tag stack=pytorch \
            --tag version=2.2 \
            --tag commit=${{ github.sha }} \
            --cleanup-script @ami-builder/cleanup.sh \
            --wait \
            --output json | jq -r '.ami_id')

          # Test AMI
          spawn test-ami $AMI \
            --test-script @ami-builder/test.sh \
            --auto-terminate

          # Tag as latest
          aws ec2 create-tags \
            --resources $AMI \
            --tags Key=spawn:latest,Value=true

          echo "Built and tested AMI: $AMI"
```

### Packer Integration

spawn could integrate with HashiCorp Packer:

```bash
# Generate Packer template from spawn config
spawn generate-packer-template \
  --base-ami al2023 \
  --instance-type m7i.large \
  --provisioner @install.sh \
  --output packer.json

# Or use Packer directly
packer build packer.json
```

---

## 10. Implementation Roadmap

### Phase 1: Enhanced Tagging (Minimal)
- ✅ Already have `--ami` flag
- ✅ Document tagging conventions
- ⬜ Add helper to tag existing AMIs: `spawn tag-ami`

### Phase 2: AMI Creation (High Value)
- ⬜ `spawn create-ami` - Create AMI from instance
- ⬜ Auto-cleanup support
- ⬜ Wait for AMI availability
- ⬜ Automatic tagging with spawn conventions

### Phase 3: AMI Discovery (Quality of Life)
- ⬜ `spawn list-amis` - List spawn-managed AMIs
- ⬜ Filter by stack, version, arch
- ⬜ Smart AMI selection: `--ami-stack pytorch --ami-version 2.2`

### Phase 4: Lifecycle Management (Operations)
- ⬜ `spawn deprecate-ami` - Mark as deprecated
- ⬜ `spawn cleanup-amis` - Deregister old AMIs
- ⬜ Age-based cleanup policies

### Phase 5: Sharing & Replication (Multi-Account)
- ⬜ `spawn share-ami` - Share across accounts
- ⬜ `spawn replicate-ami` - Copy to regions

### Phase 6: Testing & CI/CD (Advanced)
- ⬜ `spawn test-ami` - Automated validation
- ⬜ `spawn wait` - Wait for conditions
- ⬜ GitHub Actions examples

---

## 11. Example Workflows

### Workflow 1: Create PyTorch GPU AMI

```bash
# Step 1: Launch GPU instance with AL2023
spawn launch \
  --instance-type g5.xlarge \
  --name pytorch-builder \
  --ttl 2h

# Step 2: Connect and install stack
spawn connect pytorch-builder

# Install NVIDIA drivers (pre-installed on GPU AMI)
nvidia-smi

# Install PyTorch
pip3 install torch torchvision torchaudio \
  --index-url https://download.pytorch.org/whl/cu121

# Install additional tools
pip3 install transformers accelerate datasets wandb

# Test installation
python3 -c "import torch; print(torch.cuda.is_available())"

# Clean up
sudo yum clean all
rm -rf ~/.bash_history ~/.cache
history -c
exit

# Step 3: Create AMI
spawn create-ami pytorch-builder \
  --name pytorch-2.2-cuda12-$(date +%Y%m%d) \
  --tag stack=pytorch \
  --tag version=2.2 \
  --tag cuda-version=12.1 \
  --tag arch=x86_64 \
  --tag gpu=true \
  --wait

# Step 4: Test AMI
AMI=$(spawn list-amis --stack pytorch --latest --json | jq -r '.[0].ami_id')

spawn test-ami $AMI \
  --instance-type g5.xlarge \
  --test-script @validate-pytorch.sh \
  --auto-terminate

# Step 5: Use AMI
spawn launch \
  --instance-type g5.xlarge \
  --ami $AMI \
  --name training-job \
  --ttl 8h
```

### Workflow 2: Genomics Pipeline AMI

```bash
# Launch base
spawn launch --instance-type r7i.4xlarge --name genomics-builder

# Install tools
spawn connect genomics-builder

sudo yum install -y gcc-c++ make zlib-devel bzip2-devel xz-devel
wget https://github.com/samtools/samtools/releases/download/1.19/samtools-1.19.tar.bz2
tar xvf samtools-1.19.tar.bz2
cd samtools-1.19
./configure --prefix=/usr/local
make -j8
sudo make install

# Similar for bwa, bcftools, etc.
# ...

exit

# Create AMI
spawn create-ami genomics-builder \
  --name genomics-pipeline-$(date +%Y%m%d) \
  --tag stack=genomics \
  --tag version=1.0 \
  --tag tools="samtools-1.19,bwa-0.7.17,bcftools-1.19"

# Launch job array with custom AMI
AMI=$(spawn list-amis --stack genomics --latest --json | jq -r '.[0].ami_id')

spawn launch \
  --count 22 \
  --job-array-name chr-processing \
  --instance-type r7i.4xlarge \
  --ami $AMI \
  --ttl 12h \
  --command "process-chr.sh \$JOB_ARRAY_INDEX"
```

### Workflow 3: Multi-Region Deployment

```bash
# Create AMI in us-east-1
spawn launch --instance-type m7i.large --name webapp-builder --region us-east-1
# ... install stack ...
AMI_US_EAST=$(spawn create-ami webapp-builder --name webapp-v1.2.0 --wait --output json | jq -r '.ami_id')

# Replicate to other regions
spawn replicate-ami $AMI_US_EAST \
  --to-region eu-west-1 \
  --to-region ap-southeast-1 \
  --to-region us-west-2 \
  --wait

# Launch in each region
for region in us-east-1 eu-west-1 ap-southeast-1 us-west-2; do
  AMI=$(spawn list-amis --stack webapp --version 1.2.0 --region $region --json | jq -r '.[0].ami_id')

  spawn launch \
    --instance-type m7i.large \
    --ami $AMI \
    --region $region \
    --name webapp-$region
done
```

---

## 12. Best Practices

### AMI Creation

**DO:**
- ✅ Clean up before creating AMI (logs, temp files, bash history)
- ✅ Use consistent naming: `stack-version-variant-date`
- ✅ Tag comprehensively (stack, version, arch, etc.)
- ✅ Test AMI before using in production
- ✅ Document what's installed (`/opt/README.txt`)
- ✅ Use cloud-init for instance-specific config

**DON'T:**
- ❌ Include secrets/credentials in AMI
- ❌ Include user-specific files (~/.ssh, ~/.aws)
- ❌ Leave SSH host keys (they should regenerate)
- ❌ Create AMIs from production instances (use builders)
- ❌ Skip testing

### AMI Usage

**DO:**
- ✅ Use latest non-deprecated version
- ✅ Pin AMI version for critical workloads
- ✅ Monitor for deprecation warnings
- ✅ Test new AMI versions before migrating
- ✅ Keep AMI inventory documented

**DON'T:**
- ❌ Hard-code AMI IDs in scripts (they're region-specific)
- ❌ Use deprecated AMIs
- ❌ Forget to replicate to needed regions
- ❌ Mix AMI versions in a cluster

### AMI Maintenance

**DO:**
- ✅ Rebuild AMIs monthly (security patches)
- ✅ Deprecate old versions after testing new ones
- ✅ Clean up unused AMIs quarterly
- ✅ Track AMI storage costs
- ✅ Automate AMI builds (CI/CD)

**DON'T:**
- ❌ Let AMIs accumulate indefinitely
- ❌ Delete AMIs still in use
- ❌ Skip deprecation warnings

---

## 13. Cost Optimization

### Storage Costs

**Tracking:**
```bash
# Calculate AMI storage costs
spawn list-amis --json | jq '
  map(.size_gb * 0.05) |
  add |
  "Total monthly cost: $\(.)"
'

# Find expensive AMIs
spawn list-amis --sort-by size --reverse
```

**Optimization:**
- Delete unused AMIs > 180 days old
- Keep only 2-3 versions per stack
- Compress AMIs where possible (filesystem choice matters)
- Share AMIs instead of duplicating

### Launch Costs

**AMI vs user-data trade-off:**

| Factor | AMI | user-data |
|--------|-----|-----------|
| First launch | Slower (AMI creation) | Faster (no AMI) |
| Subsequent launches | **Much faster** | Same speed |
| Storage cost | **$0.05/GB/month** | $0 |
| Reliability | **More reliable** | Depends on internet |
| Reproducibility | **Exact versions** | May vary |

**Break-even calculation:**
```
AMI cost per month: $1.50 (30GB AMI)
Time saved per launch: 10 minutes
Value of time: $60/hour = $10 per 10 minutes

Break-even: 1.5 launches per month
```

**Recommendation**: Use AMIs if launching more than 2-3 times per month.

---

## 14. Security Considerations

### AMI Hardening

**Pre-creation checklist:**
- Remove SSH authorized_keys
- Remove bash history
- Remove cloud-init logs (if not needed)
- Remove temporary credentials
- Disable root SSH
- Clear package manager cache
- Remove old kernel versions

**Automated hardening:**
```bash
# hardening-script.sh
#!/bin/bash

# Remove histories
rm -f ~/.bash_history /root/.bash_history
history -c

# Remove SSH keys (keep authorized_keys from AWS)
rm -f ~/.ssh/id_*
rm -f /root/.ssh/id_*

# Remove cloud-init logs
sudo rm -rf /var/log/cloud-init*

# Clear logs
sudo truncate -s 0 /var/log/*.log

# Remove temp files
sudo rm -rf /tmp/*
sudo rm -rf /var/tmp/*

# Package cleanup
sudo yum clean all
sudo rm -rf /var/cache/yum

echo "Hardening complete"
```

### Vulnerability Management

**Patching strategy:**
- Rebuild AMIs monthly with latest patches
- Use automated scanning (Amazon Inspector)
- Track CVEs in base OS and dependencies
- Deprecate AMIs with known vulnerabilities

---

## 15. Comparison with Alternatives

### AMI vs Container Images

| Feature | AMI | Container |
|---------|-----|-----------|
| Boot time | Slower (~30s) | **Faster (~1s)** |
| Image size | Larger (GBs) | **Smaller (MBs-GB)** |
| Portability | AWS-only | **Cross-cloud** |
| System-level | **Yes** | Limited |
| Kernel modules | **Yes** | No |
| GPU drivers | **Yes** | Requires host |
| Use case | Full VMs | **Microservices** |

**Recommendation**:
- Use AMIs for: GPU workloads, system-level changes, VMs
- Use containers for: Microservices, portable workloads

### AMI vs Packer

**spawn AMI tools** (proposed):
- ✅ Simple, integrated with spawn workflow
- ✅ No additional tools required
- ❌ Less flexible than Packer
- ❌ AWS-only

**Packer:**
- ✅ Very flexible, production-grade
- ✅ Multi-cloud support
- ✅ Advanced provisioners
- ❌ Steeper learning curve
- ❌ Separate tool to learn

**Recommendation**:
- Use spawn tools for: Simple AMIs, quick iterations
- Use Packer for: Complex builds, multi-cloud, CI/CD

---

## 16. Questions to Explore

### Design Decisions

1. **Command naming:**
   - `spawn create-ami` vs `spawn ami create`?
   - `spawn list-amis` vs `spawn ami list`?

2. **Smart selection:**
   - How to handle multiple matching AMIs?
   - Prefer latest by default?
   - Allow version ranges (">= 2.0, < 3.0")?

3. **Safety:**
   - Require confirmation for destructive operations?
   - Dry-run mode by default?
   - Protect production AMIs?

4. **Integration:**
   - Should spawn integrate with Packer?
   - Support other AMI builders?
   - Integration with CI/CD platforms?

5. **Storage:**
   - Track AMI metadata in DynamoDB?
   - Local cache of AMI listings?
   - Version history tracking?

---

## Summary

AMI management is critical for spawn's usability with complex software stacks. A comprehensive AMI lifecycle system should support:

1. **Creation**: Easy AMI creation from running instances
2. **Tagging**: Consistent metadata for discovery
3. **Discovery**: Find AMIs by stack, version, architecture
4. **Versioning**: Semantic versioning with deprecation
5. **Testing**: Automated validation before use
6. **Sharing**: Cross-account and public sharing
7. **Replication**: Multi-region deployment
8. **Cleanup**: Automated deregistration of old AMIs

**Minimal viable implementation:**
- `spawn create-ami` - Create AMI with auto-tagging
- `spawn list-amis` - Discover spawn-managed AMIs
- Smart AMI selection with `--ami-stack` flag

**Future enhancements:**
- Automated testing pipeline
- CI/CD integration
- Packer integration
- Cost tracking and optimization

This enables the "build once, launch many" workflow that's essential for production use of spawn.
