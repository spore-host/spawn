# Data Staging Guide

**Table of Contents**
- [Introduction](#introduction)
- [Quick Start](#quick-start)
- [Command Reference](#command-reference)
- [Cost Optimization](#cost-optimization)
- [Regional Buckets](#regional-buckets)
- [Integration with Sweeps](#integration-with-sweeps)
- [Examples](#examples)
- [Advanced Topics](#advanced-topics)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

---

## Introduction

### What is Data Staging?

Data staging is spawn's solution for efficiently distributing large datasets to instances across multiple AWS regions. Instead of each instance downloading data from a single source (incurring cross-region transfer costs), staging replicates data once to regional buckets, allowing instances to download from their local region for free.

**The Problem:**
```
Without Staging:
  Source Bucket (us-east-1)
       ↓ $0.09/GB
  ┌────┴────┬────┬────┐
  ↓         ↓    ↓    ↓
100 instances in us-west-2
= 100GB × 100 instances × $0.09 = $900
```

**The Solution:**
```
With Staging:
  Source Bucket (us-east-1)
       ↓ $0.02/GB (one-time)
  Staging Bucket (us-west-2)
       ↓ FREE
  ┌────┴────┬────┬────┐
  ↓         ↓    ↓    ↓
100 instances in us-west-2
= 100GB × $0.02 = $2
Savings: $898 (99.8%)
```

### When to Use Data Staging

**Use Staging When:**
- Dataset > 10GB per instance
- Running instances in multiple regions
- > 5 instances per region will use the data
- Data doesn't change frequently

**Example Scenarios:**
- **Reference genomes** (100GB+) for bioinformatics
- **Model weights** (10-50GB) for ML inference
- **Training datasets** (500GB+) for distributed training
- **Simulation inputs** (large meshes, parameter files)

**Skip Staging When:**
- Small datasets (< 1GB)
- Single region deployments
- Data changes between tasks
- Only 1-2 instances need the data

### Cost Savings Explained

**AWS Data Transfer Pricing:**
- **Within Region:** FREE (S3 → EC2 in same region)
- **Cross-Region:** $0.09/GB (S3 in us-east-1 → EC2 in us-west-2)
- **S3 Replication:** ~$0.02/GB (S3 → S3 across regions)

**Break-Even Calculation:**
```
Replication Cost = Data Size × Regions × $0.02
Transfer Cost = Data Size × Regions × Instances × $0.09

Break-even when: Instances > (0.02 / 0.09) ≈ 0.22

Staging is cost-effective with just 1+ instance per region!
```

---

## Quick Start

### Installation

Ensure spawn is installed and configured:

```bash
# Verify installation
spawn --version

# Configure AWS credentials (for staging buckets)
aws configure
```

### Your First Staging

1. **Stage a dataset:**

```bash
# Upload 100GB reference genome to 2 regions
spawn stage upload ./reference-genome.fasta \
  --regions us-east-1,us-west-2 \
  --dest /mnt/data/reference.fasta
```

Output:
```
Staging Data: reference-genome.fasta
Size: 100.0 GB
Regions: us-east-1, us-west-2
Destination: /mnt/data/reference.fasta

Uploading to primary region (us-east-1)...
████████████████████████████████████████ 100% | 2.5 GB/s | ETA: 0s

Replicating to us-west-2...
████████████████████████████████████████ 100% | 500 MB/s | ETA: 0s

✅ Staging Complete!

Staging ID: stage-abc123def456
Metadata stored in DynamoDB
TTL: 7 days (auto-cleanup on 2024-01-29)

Cost Summary:
  Upload: $0.00 (free to S3)
  Replication: $2.00 (100GB × 1 region × $0.02/GB)
  Storage: $2.30/day (100GB × 2 regions × $0.023/GB/month)
  Total (7 days): $18.10

Estimated Savings:
  vs. 10 instances downloading: $180 → $18 (90% savings)
  vs. 100 instances downloading: $1,800 → $18 (99% savings)

To use in parameter sweep:
  spawn launch --params sweep.yaml --stage-id stage-abc123def456
```

2. **Use staged data in a sweep:**

```yaml
# sweep.yaml
sweep_name: genome-analysis
count: 100
regions:
  - us-east-1
  - us-west-2
stage_id: stage-abc123def456
base_command: |
  #!/bin/bash
  # Data auto-downloaded to /mnt/data/reference.fasta
  ./analyze_genome /mnt/data/reference.fasta output.vcf
```

```bash
spawn launch --params sweep.yaml
```

3. **List and manage staged data:**

```bash
# List all staged data
spawn stage list

# Get details
spawn stage info stage-abc123def456

# Delete when done
spawn stage delete stage-abc123def456
```

---

## Command Reference

### `spawn stage upload`

Uploads and replicates data to regional staging buckets.

```bash
spawn stage upload <local-path> [OPTIONS]
```

**Arguments:**
- `<local-path>`: File or directory to stage

**Options:**
- `--regions REGIONS`: Comma-separated list of regions (default: `us-east-1,us-west-2`)
- `--dest PATH`: Destination path on instances (default: `/mnt/data/<filename>`)
- `--sweep-id ID`: Associate with sweep for tracking
- `--ttl DAYS`: Days before auto-deletion (default: 7)

**Examples:**

```bash
# Basic upload
spawn stage upload dataset.tar.gz

# Multiple regions
spawn stage upload model-weights.pt \
  --regions us-east-1,us-west-2,eu-west-1

# Custom destination
spawn stage upload data/ \
  --dest /opt/app/data

# Associate with sweep
spawn stage upload input.dat \
  --sweep-id sweep-xyz789 \
  --ttl 14
```

**Output:**
- Staging ID for reference
- Cost summary
- Estimated savings
- Usage instructions

---

### `spawn stage list`

Lists all staged data with metadata.

```bash
spawn stage list [OPTIONS]
```

**Options:**
- `--sweep-id ID`: Filter by sweep ID
- `--region REGION`: Filter by region
- `--format FORMAT`: Output format (table, json, yaml)

**Examples:**

```bash
# List all staged data
spawn stage list

# Filter by sweep
spawn stage list --sweep-id sweep-xyz789

# JSON output
spawn stage list --format json
```

**Output:**
```
STAGING ID          SIZE    REGIONS  UPLOADED    TTL   SWEEP ID
stage-abc123def456  100GB   2        2024-01-22  7d    sweep-xyz789
stage-ghi789jkl012  50GB    3        2024-01-21  7d    -
stage-mno345pqr678  25GB    1        2024-01-20  14d   sweep-aaa111
```

---

### `spawn stage info`

Shows detailed information about staged data.

```bash
spawn stage info <staging-id>
```

**Example:**

```bash
spawn stage info stage-abc123def456
```

**Output:**
```
Staging ID: stage-abc123def456
File: reference-genome.fasta
Size: 100.0 GB (107,374,182,400 bytes)
SHA256: a1b2c3d4e5f6...
Uploaded: 2024-01-22 10:30:15 UTC
Expires: 2024-01-29 10:30:15 UTC (7 days)
Sweep ID: sweep-xyz789

Regions:
  us-east-1: s3://spawn-staging-us-east-1/stage-abc123def456/reference-genome.fasta
  us-west-2: s3://spawn-staging-us-west-2/stage-abc123def456/reference-genome.fasta

Destination: /mnt/data/reference.fasta

Instances Using Data: 47
  us-east-1: 23 instances
  us-west-2: 24 instances

Cost Summary:
  Replication: $2.00
  Storage (to date): $4.60
  Total: $6.60

Savings:
  vs. direct downloads: $423.00 (98% savings)
```

---

### `spawn stage estimate`

Estimates cost savings from staging vs. direct downloads.

```bash
spawn stage estimate [OPTIONS]
```

**Options:**
- `--data-size-gb GB`: Dataset size in GB
- `--instances COUNT`: Number of instances per region
- `--regions REGIONS`: Comma-separated region list

**Examples:**

```bash
# Estimate for 100GB dataset
spawn stage estimate \
  --data-size-gb 100 \
  --instances 10 \
  --regions us-east-1,us-west-2
```

**Output:**
```
Dataset: 100 GB
Instances: 10 per region
Regions: 2 (us-east-1, us-west-2)

Scenario A: Direct Downloads (No Staging)
  Cross-region transfers: 100GB × 1 region × 10 instances = 1,000 GB
  Cost: 1,000 GB × $0.09/GB = $90.00

Scenario B: Data Staging
  Replication: 100GB × 1 region × $0.02/GB = $2.00
  Storage (7 days): 100GB × 2 regions × $0.023/GB/month × (7/30) = $1.07
  Downloads (within region): FREE
  Total: $3.07

Savings: $86.93 (96.6% reduction)

Break-even: 1 instance per region
Recommendation: ✅ Use staging (highly cost-effective)
```

---

### `spawn stage delete`

Deletes staged data from all regions.

```bash
spawn stage delete <staging-id> [OPTIONS]
```

**Options:**
- `--yes`, `-y`: Skip confirmation prompt

**Examples:**

```bash
# Delete with confirmation
spawn stage delete stage-abc123def456

# Force delete
spawn stage delete stage-abc123def456 --yes
```

**Output:**
```
Deleting staged data: stage-abc123def456
File: reference-genome.fasta
Size: 100.0 GB
Regions: us-east-1, us-west-2

⚠️  This will delete data from all regions and cannot be undone.

Continue? [y/N]: y

Deleting from us-east-1... ✓
Deleting from us-west-2... ✓
Removing metadata... ✓

✅ Staging deleted
```

---

## Cost Optimization

### Cost Breakdown

**Three Cost Components:**

1. **Upload to S3:**
   - Cost: FREE
   - Bandwidth: Typically 100-500 MB/s

2. **S3 Cross-Region Replication:**
   - Cost: ~$0.02/GB per destination region
   - Bandwidth: 50-200 MB/s per region
   - Example: 100GB to 3 regions = $6.00

3. **S3 Storage:**
   - Cost: $0.023/GB/month (Standard tier)
   - Example: 100GB × 2 regions × 7 days = $1.07

**Total Cost Formula:**
```
Total = (Data_GB × Regions × $0.02) + (Data_GB × Regions × $0.023/month × Days/30)
```

### When Staging Saves Money

**Calculator:**

```python
def staging_cost(data_gb, regions, days=7):
    replication = data_gb * (regions - 1) * 0.02
    storage = data_gb * regions * 0.023 * (days / 30)
    return replication + storage

def direct_cost(data_gb, regions, instances):
    # Instances in non-primary regions pay cross-region transfer
    return data_gb * (regions - 1) * instances * 0.09

def savings(data_gb, regions, instances, days=7):
    staged = staging_cost(data_gb, regions, days)
    direct = direct_cost(data_gb, regions, instances)
    return direct - staged, (direct - staged) / direct * 100

# Example: 100GB, 2 regions, 10 instances
data_gb = 100
regions = 2
instances = 10

saved, pct = savings(data_gb, regions, instances)
print(f"Savings: ${saved:.2f} ({pct:.1f}%)")
# Output: Savings: $86.93 (96.6%)
```

**Break-Even Analysis:**

| Dataset Size | Regions | Break-Even Instances |
|--------------|---------|---------------------|
| 10 GB | 2 | 1 |
| 50 GB | 2 | 1 |
| 100 GB | 2 | 1 |
| 100 GB | 3 | 1 |
| 500 GB | 3 | 1 |

**Conclusion:** Staging is cost-effective with **just 1 instance per region** for any dataset > 10GB.

---

### Cost Examples

#### Small Dataset (10GB)

```bash
spawn stage estimate --data-size-gb 10 --instances 5 --regions us-east-1,us-west-2
```

```
Staging: $0.20 (replication) + $0.11 (storage) = $0.31
Direct: 10GB × 1 region × 5 instances × $0.09 = $4.50
Savings: $4.19 (93%)
```

#### Medium Dataset (100GB)

```bash
spawn stage estimate --data-size-gb 100 --instances 20 --regions us-east-1,us-west-2,eu-west-1
```

```
Staging: $4.00 (replication) + $1.61 (storage) = $5.61
Direct: 100GB × 2 regions × 20 instances × $0.09 = $360
Savings: $354.39 (98.4%)
```

#### Large Dataset (1TB)

```bash
spawn stage estimate --data-size-gb 1000 --instances 100 --regions us-east-1,us-west-2,eu-west-1,ap-south-1
```

```
Staging: $60.00 (replication) + $21.43 (storage) = $81.43
Direct: 1000GB × 3 regions × 100 instances × $0.09 = $27,000
Savings: $26,918.57 (99.7%)
```

---

## Regional Buckets

### Available Regions

Spawn maintains staging buckets in all major AWS regions:

| Region Code | Region Name | Bucket Name |
|-------------|-------------|-------------|
| us-east-1 | US East (N. Virginia) | spawn-staging-us-east-1 |
| us-east-2 | US East (Ohio) | spawn-staging-us-east-2 |
| us-west-1 | US West (N. California) | spawn-staging-us-west-1 |
| us-west-2 | US West (Oregon) | spawn-staging-us-west-2 |
| eu-west-1 | Europe (Ireland) | spawn-staging-eu-west-1 |
| eu-west-2 | Europe (London) | spawn-staging-eu-west-2 |
| eu-central-1 | Europe (Frankfurt) | spawn-staging-eu-central-1 |
| ap-south-1 | Asia Pacific (Mumbai) | spawn-staging-ap-south-1 |
| ap-southeast-1 | Asia Pacific (Singapore) | spawn-staging-ap-southeast-1 |
| ap-southeast-2 | Asia Pacific (Sydney) | spawn-staging-ap-southeast-2 |
| ap-northeast-1 | Asia Pacific (Tokyo) | spawn-staging-ap-northeast-1 |

### Bucket Naming Convention

```
spawn-staging-{region}/{staging-id}/{filename}
```

**Example:**
```
s3://spawn-staging-us-east-1/stage-abc123def456/reference-genome.fasta
s3://spawn-staging-us-west-2/stage-abc123def456/reference-genome.fasta
```

### Lifecycle Policies

All staging buckets have automatic cleanup:

- **Default TTL:** 7 days
- **Custom TTL:** 1-90 days (via `--ttl` flag)
- **Cleanup:** Daily at 00:00 UTC
- **Metadata:** Removed from DynamoDB on cleanup

**View expiration:**
```bash
spawn stage info stage-abc123def456
# Expires: 2024-01-29 10:30:15 UTC (7 days)
```

**Extend TTL:**
```bash
spawn stage update stage-abc123def456 --ttl 14
# TTL extended to 14 days
```

---

### Manual Setup (Advanced)

If using a custom AWS account, set up staging buckets manually:

```bash
# Create staging bucket
aws s3 mb s3://my-spawn-staging-us-east-1 --region us-east-1

# Enable versioning
aws s3api put-bucket-versioning \
  --bucket my-spawn-staging-us-east-1 \
  --versioning-configuration Status=Enabled

# Add lifecycle policy
cat > lifecycle.json <<EOF
{
  "Rules": [{
    "Id": "cleanup-after-7-days",
    "Status": "Enabled",
    "Expiration": { "Days": 7 },
    "NoncurrentVersionExpiration": { "Days": 1 }
  }]
}
EOF

aws s3api put-bucket-lifecycle-configuration \
  --bucket my-spawn-staging-us-east-1 \
  --lifecycle-configuration file://lifecycle.json

# Configure spawn
spawn config set staging.bucket.us-east-1 my-spawn-staging-us-east-1
```

---

## Integration with Sweeps

### Automatic Download

When launching with `--stage-id`, spawn automatically:
1. Detects instance region
2. Downloads from regional bucket
3. Verifies SHA256 checksum
4. Places file at destination path

**Parameter File:**
```yaml
sweep_name: genome-pipeline
count: 50
regions:
  - us-east-1: 25
  - us-west-2: 25
stage_id: stage-abc123def456
base_command: |
  #!/bin/bash
  # Data available at /mnt/data/reference.fasta
  ./analyze /mnt/data/reference.fasta output.vcf
```

**Launch:**
```bash
spawn launch --params sweep.yaml
```

**Behind the Scenes:**
```bash
# On each instance (automatically)
STAGING_ID="stage-abc123def456"
REGION=$(ec2-metadata --availability-zone | sed 's/[a-z]$//')
BUCKET="spawn-staging-${REGION}"

aws s3 cp "s3://${BUCKET}/${STAGING_ID}/reference.fasta" \
  /mnt/data/reference.fasta

# Verify checksum
echo "${EXPECTED_SHA256}  /mnt/data/reference.fasta" | sha256sum -c
```

---

### Multi-Region Sweeps

**Use Case:** Run sweep across multiple regions with same dataset

```yaml
sweep_name: ml-training
count: 100
distribution: even  # or: proportional, cheapest
regions:
  - us-east-1
  - us-west-2
  - eu-west-1
stage_id: stage-abc123def456  # Data available in all regions
instance_type: p3.2xlarge
base_command: |
  #!/bin/bash
  # Training data pre-downloaded
  python train.py \
    --data /mnt/data/training-set.tar.gz \
    --output /mnt/results/model-${SWEEP_INDEX}.pt
```

**Launch:**
```bash
spawn launch --params sweep.yaml --spot
```

**Result:**
- ~33 instances per region
- Each downloads from local staging bucket (FREE)
- Total data transfer cost: $0

---

### Multiple Datasets

Stage multiple datasets for a single sweep:

```bash
# Stage reference genome
genome_id=$(spawn stage upload reference.fasta --regions us-east-1,us-west-2 --dest /data/reference.fasta)

# Stage training data
train_id=$(spawn stage upload training.tar.gz --regions us-east-1,us-west-2 --dest /data/training.tar.gz)

# Create parameter file
cat > sweep.yaml <<EOF
sweep_name: analysis
stage_ids:
  - $genome_id
  - $train_id
regions:
  - us-east-1
  - us-west-2
base_command: |
  #!/bin/bash
  # Both datasets available
  ls -lh /data/
  # reference.fasta
  # training.tar.gz
  ./analyze.sh
EOF

spawn launch --params sweep.yaml
```

---

## Examples

### Example 1: Bioinformatics Reference Genome

**Scenario:** 100 researchers analyzing different samples against the same 100GB reference genome

```bash
# Stage reference genome once
spawn stage upload hg38-reference.fasta \
  --regions us-east-1,us-west-2 \
  --dest /mnt/data/reference.fasta \
  --ttl 30

# Output: stage-ref-hg38-xyz

# Each researcher uses the staged genome
cat > my-analysis.yaml <<EOF
sweep_name: my-samples
stage_id: stage-ref-hg38-xyz
count: 10
base_command: |
  #!/bin/bash
  bwa mem /mnt/data/reference.fasta sample-${SWEEP_INDEX}.fastq > aligned-${SWEEP_INDEX}.sam
EOF

spawn launch --params my-analysis.yaml --spot
```

**Cost Savings:**
- Without staging: 100GB × 100 researchers × $0.09 = $900
- With staging: $2 (one-time) + $6.90 (30 days storage) = $8.90
- **Savings: $891.10 (99%)**

---

### Example 2: ML Model Inference

**Scenario:** Deploy trained model (50GB) for distributed inference across 3 regions

```bash
# Stage model weights
spawn stage upload model-v3.pt \
  --regions us-east-1,us-west-2,eu-west-1 \
  --dest /opt/model/weights.pt

# Output: stage-model-v3-abc

# Launch inference fleet
cat > inference.yaml <<EOF
sweep_name: inference-fleet
stage_id: stage-model-v3-abc
count: 300
regions:
  - us-east-1: 100
  - us-west-2: 100
  - eu-west-1: 100
instance_type: p3.2xlarge
base_command: |
  #!/bin/bash
  python infer.py \
    --model /opt/model/weights.pt \
    --input s3://inference-inputs/batch-${SWEEP_INDEX}/ \
    --output s3://inference-outputs/results-${SWEEP_INDEX}/
EOF

spawn launch --params inference.yaml --spot
```

**Cost Savings:**
- Without staging: 50GB × 2 regions × 300 instances × $0.09 = $2,700
- With staging: 50GB × 2 regions × $0.02 = $2
- **Savings: $2,698 (99.9%)**

---

### Example 3: Simulation Input Files

**Scenario:** Complex simulation with large mesh files (200GB) across multiple scenarios

```bash
# Stage mesh and input files
spawn stage upload simulation-inputs/ \
  --regions us-east-1,us-west-2 \
  --dest /mnt/inputs

# Output: stage-sim-inputs-def

# Run parameter sweep
cat > simulation.yaml <<EOF
sweep_name: parameter-sweep
stage_id: stage-sim-inputs-def
params_file: parameters.csv  # 1000 parameter combinations
base_command: |
  #!/bin/bash
  cd /mnt/inputs
  ./simulate --params ${PARAM_SET} --output /mnt/results/run-${SWEEP_INDEX}.dat
EOF

spawn launch --params simulation.yaml --spot
```

---

## Advanced Topics

### SHA256 Integrity Verification

All staged data includes SHA256 checksums for integrity verification:

```bash
# Upload with automatic checksum
spawn stage upload data.tar.gz --regions us-east-1,us-west-2

# Checksum stored in metadata
spawn stage info stage-abc123def456
# SHA256: a1b2c3d4e5f6...

# Verified automatically on instance
# If checksum fails, instance terminates with error
```

**Manual verification:**
```bash
# On instance
sha256sum /mnt/data/data.tar.gz
# Compare with stored checksum
```

---

### Metadata Tracking

Staging metadata is stored in DynamoDB for fast lookups:

**Metadata Schema:**
```json
{
  "staging_id": "stage-abc123def456",
  "filename": "reference.fasta",
  "size_bytes": 107374182400,
  "sha256": "a1b2c3d4e5f6...",
  "regions": ["us-east-1", "us-west-2"],
  "destination": "/mnt/data/reference.fasta",
  "uploaded_at": "2024-01-22T10:30:15Z",
  "expires_at": "2024-01-29T10:30:15Z",
  "sweep_id": "sweep-xyz789",
  "instance_count": 47,
  "download_count": 47,
  "cost": {
    "replication": 2.00,
    "storage_per_day": 0.46
  }
}
```

**Query metadata:**
```bash
# Get all staging for a sweep
spawn stage list --sweep-id sweep-xyz789

# Get staging details
spawn stage info stage-abc123def456

# Export to JSON
spawn stage list --format json > staging.json
```

---

### Cross-Account Staging

Share staged data across AWS accounts:

```bash
# Account A: Stage data with cross-account access
spawn stage upload dataset.tar.gz \
  --regions us-east-1,us-west-2 \
  --allow-accounts 123456789012,987654321098

# Account B: Use staged data
spawn launch --params sweep.yaml \
  --stage-id stage-abc123def456 \
  --stage-account 111111111111
```

**IAM Policy (Account B instances):**
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["s3:GetObject"],
    "Resource": "arn:aws:s3:::spawn-staging-*/stage-abc123def456/*"
  }]
}
```

---

### Compression and Decompression

Spawn automatically handles compressed files:

```bash
# Upload compressed (recommended for large datasets)
tar czf dataset.tar.gz dataset/
spawn stage upload dataset.tar.gz --dest /mnt/data/dataset.tar.gz

# Auto-decompress on instances
cat > sweep.yaml <<EOF
base_command: |
  #!/bin/bash
  # Decompress staged data
  cd /mnt/data
  tar xzf dataset.tar.gz
  ./process dataset/
EOF
```

**Compression Ratios:**
| File Type | Typical Compression | Example |
|-----------|---------------------|---------|
| Text/CSV | 80-90% | 100GB → 10GB |
| FASTQ (genomics) | 70-80% | 100GB → 25GB |
| Images (PNG/JPG) | 0-10% | Already compressed |
| Binary data | 30-50% | 100GB → 60GB |

**Recommendation:** Always compress text-based data before staging.

---

## Best Practices

### File Size Recommendations

| Dataset Size | Recommendation | Reasoning |
|--------------|----------------|-----------|
| < 1 GB | Skip staging | Transfer cost negligible |
| 1-10 GB | Optional | Breaks even at ~5 instances |
| 10-100 GB | **Recommended** | Significant savings |
| 100GB-1TB | **Highly recommended** | Massive savings (99%+) |
| > 1 TB | **Required** | Transfer may be impractical |

---

### Region Selection Strategy

**Choose regions based on:**

1. **Instance availability** - Where will instances run?
2. **Data sources** - Where is data generated/stored?
3. **Compliance** - Data residency requirements
4. **Cost** - Some regions have lower prices

**Common Patterns:**

**US-Only:**
```bash
--regions us-east-1,us-west-2
```

**Global:**
```bash
--regions us-east-1,us-west-2,eu-west-1,ap-south-1
```

**EU-Compliant:**
```bash
--regions eu-west-1,eu-west-2,eu-central-1
```

---

### Cleanup Management

**Automatic Cleanup:**
- Default: 7 days
- Configurable: 1-90 days
- Triggered: Daily at 00:00 UTC

**Manual Cleanup:**
```bash
# Delete immediately when done
spawn stage delete stage-abc123def456 --yes

# Delete all expired staging
spawn stage cleanup --expired

# Delete by sweep
spawn stage cleanup --sweep-id sweep-xyz789
```

**Cost Monitoring:**
```bash
# View storage costs
spawn stage cost

# View per-staging costs
spawn stage cost --staging-id stage-abc123def456
```

---

### Cost Monitoring

**Track staging costs over time:**

```bash
# Daily staging report
spawn stage cost --since yesterday

# Monthly staging report
spawn stage cost --since "30 days ago"

# Cost by sweep
spawn stage cost --sweep-id sweep-xyz789

# Export for analysis
spawn stage cost --format csv > staging-costs.csv
```

**Example Output:**
```
Staging Cost Report (Last 30 Days)

Total Staging Operations: 15
Total Data Staged: 2.5 TB
Total Cost: $78.50

Breakdown:
  Replication: $50.00 (64%)
  Storage: $28.50 (36%)

Top Staging by Cost:
  1. stage-abc123 (500GB, 4 regions): $45.00
  2. stage-def456 (300GB, 3 regions): $18.00
  3. stage-ghi789 (200GB, 2 regions): $8.50

Savings vs. Direct Transfer: $12,450 (99.4%)
```

---

## Troubleshooting

### Upload Failures

**Error:** `upload failed: connection timeout`

**Cause:** Network issues, large file, slow connection

**Solution:**
```bash
# Use multipart upload (automatic for files > 5GB)
spawn stage upload large-file.tar.gz --multipart

# Increase timeout
spawn stage upload large-file.tar.gz --timeout 3600

# Resume failed upload
spawn stage resume stage-abc123def456
```

---

### Replication Errors

**Error:** `replication failed: access denied in us-west-2`

**Cause:** Missing S3 permissions in destination region

**Solution:**
```bash
# Check IAM permissions
aws s3 ls s3://spawn-staging-us-west-2/

# Required permissions:
# - s3:PutObject
# - s3:PutObjectAcl
# - s3:ListBucket

# Fix permissions (admin)
aws iam attach-user-policy \
  --user-name spawn-user \
  --policy-arn arn:aws:iam::aws:policy/AmazonS3FullAccess
```

---

### Download Failures on Instances

**Error:** `download failed: 403 Forbidden`

**Cause:** Instance IAM role lacks S3 GetObject permission

**Solution:**
```bash
# Launch with proper IAM role
spawn launch --params sweep.yaml \
  --iam-policy s3:GetObject \
  --iam-policy s3:ListBucket

# Or use managed policy
spawn launch --params sweep.yaml \
  --iam-managed-policies arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

---

### Checksum Verification Failures

**Error:** `SHA256 checksum mismatch`

**Cause:** File corruption during transfer

**Solution:**
```bash
# Re-download with verification
aws s3 cp s3://spawn-staging-us-east-1/stage-abc123/file.tar.gz /tmp/file.tar.gz --no-progress

# Verify manually
echo "a1b2c3d4e5f6...  /tmp/file.tar.gz" | sha256sum -c

# If still failing, re-upload
spawn stage delete stage-abc123def456 --yes
spawn stage upload file.tar.gz --regions us-east-1,us-west-2
```

---

### Cost Discrepancies

**Issue:** Actual costs higher than estimate

**Causes:**
1. Data transfer out (instances downloading)
2. S3 API requests
3. Early deletion fees
4. Storage class mismatch

**Investigation:**
```bash
# Check actual costs in AWS Cost Explorer
aws ce get-cost-and-usage \
  --time-period Start=2024-01-01,End=2024-01-31 \
  --granularity MONTHLY \
  --metrics BlendedCost \
  --filter file://staging-filter.json

# staging-filter.json
{
  "Tags": {
    "Key": "StagingID",
    "Values": ["stage-abc123def456"]
  }
}

# Compare with estimate
spawn stage estimate \
  --data-size-gb 100 \
  --instances 50 \
  --regions us-east-1,us-west-2
```

---

## Getting Help

**Documentation:**
- Main docs: https://github.com/spore-host/spore-host
- Cost calculator: `spawn stage estimate --help`
- Examples: `spawn/examples/staging/`

**Issues:**
- Report bugs: https://github.com/spore-host/spore-host/issues
- Feature requests: Tag with `enhancement`

**Support:**
```bash
# Include in bug report:
spawn --version
spawn stage list --format json
aws s3 ls s3://spawn-staging-us-east-1/
```

---

## Summary

**Data Staging enables:**
✅ 90-99% cost reduction for multi-region data distribution
✅ Automatic regional bucket selection
✅ Integrity verification (SHA256)
✅ Automatic cleanup after 7 days
✅ Seamless integration with parameter sweeps

**Use staging when:**
- Dataset > 10GB
- Multiple regions
- Multiple instances per region
- Data doesn't change between tasks

**Getting started:**
```bash
# 1. Stage data
spawn stage upload dataset.tar.gz --regions us-east-1,us-west-2

# 2. Launch sweep with staged data
spawn launch --params sweep.yaml --stage-id stage-abc123

# 3. Clean up when done
spawn stage delete stage-abc123 --yes
```
