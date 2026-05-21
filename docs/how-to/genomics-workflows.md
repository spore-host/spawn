# How-To: Run Genomics Workflows on spawn

Run bioinformatics workflows (WDL, CWL, Nextflow) on spawn as a cost-effective alternative to AWS HealthOmics.

---

## Overview

### Why Run Genomics Workflows on spawn?

**Cost Efficiency:**
- spawn using spot instances: **$0.50 per 30x WGS** (4 hours, c6i.4xlarge)
- AWS HealthOmics private: **$3.67 per run** (7x more expensive)
- AWS HealthOmics Ready2Run: **$10.00 per run** (20x more expensive)

**Flexibility:**
- Universal workflow engine support (WDL, CWL, Nextflow, Snakemake, custom)
- Full control over compute (instance types, spot/on-demand, regions)
- No vendor lock-in (standard EC2 + S3)
- Works with any workflow, not just genomics

**Transparency:**
- See exactly what you're paying for (no hidden managed fees)
- Direct access to instances for debugging
- Standard AWS infrastructure (familiar cost model)

---

## Supported Workflow Languages

| Language | Execution Engine | spawn Support | Notes |
|----------|-----------------|---------------|-------|
| **WDL** | Cromwell, miniwdl | ✅ Full | GATK Best Practices, Broad pipelines |
| **CWL** | cwltool, Toil | ✅ Full | Common Workflow Language standard |
| **Nextflow** | Nextflow | ✅ Full | nf-core pipelines (sarek, rnaseq, etc.) |
| **Snakemake** | Snakemake | ✅ Full | Not supported by HealthOmics |

**spawn Advantage:** Supports Snakemake and custom Python workflows that HealthOmics doesn't support.

---

## Architecture Patterns

### Pattern 1: Single Orchestrator Instance

**Best for:** Most genomics workflows

```
spawn instance (orchestrator)
├── Cromwell/cwltool/Nextflow running
├── Launches worker tasks via AWS Batch/EC2
└── Coordinates workflow execution
```

**Launch orchestrator:**
```bash
spawn launch \
  --instance-type c6i.2xlarge \
  --ami ami-genomics-orchestrator \
  --ttl 8h \
  --spot \
  --iam-policy batch:FullAccess,s3:FullAccess \
  --storage 200GB \
  --user-data orchestrator-setup.sh
```

---

### Pattern 2: spawn Job Arrays for Parallel Tasks

**Best for:** Embarrassingly parallel workflows (many samples)

```
spawn job array (N instances)
├── Instance 1: Process sample 1
├── Instance 2: Process sample 2
├── ...
└── Instance N: Process sample N
```

**Launch array:**
```bash
spawn launch \
  --params samples.yaml \
  --instance-type c6i.4xlarge \
  --array-size 100 \
  --spot \
  --ttl 6h
```

**samples.yaml:**
```yaml
parameters:
  sample_id: [SAMPLE001, SAMPLE002, ..., SAMPLE100]
  fastq_r1: [s3://data/S001_R1.fq.gz, ...]
  fastq_r2: [s3://data/S001_R2.fq.gz, ...]
  reference: s3://references/GRCh38.fa

instance-type: c6i.4xlarge
region: us-east-1
```

---

### Pattern 3: Hybrid (Orchestrator + spawn Arrays)

**Best for:** Complex pipelines with both sequential and parallel stages

```
Orchestrator instance
├── Stage 1: QC (spawn array, 100 samples)
├── Stage 2: Alignment (spawn array, 100 samples)
├── Stage 3: Variant calling (spawn array, 100 samples)
└── Stage 4: Joint calling (single large instance)
```

---

## WDL Workflows with Cromwell

### Setup Cromwell on spawn

**1. Create Cromwell AMI:**

```bash
# Launch instance
spawn launch --instance-type c6i.2xlarge --wait-for-ssh

# Install Cromwell
ssh instance
sudo yum install -y java-11-amazon-corretto docker
wget https://github.com/broadinstitute/cromwell/releases/download/85/cromwell-85.jar
mv cromwell-85.jar /usr/local/bin/cromwell.jar

# Configure Cromwell for AWS Batch backend
cat > cromwell.conf <<'EOF'
include required(classpath("application"))

backend {
  default = "AWSEC2"
  providers {
    AWSEC2 {
      actor-factory = "cromwell.backend.impl.aws.AwsBatchBackendLifecycleActorFactory"
      config {
        root = "s3://cromwell-executions/"
        region = "us-east-1"

        # Use spawn spot instances for cost savings
        default-runtime-attributes {
          queueArn = "arn:aws:batch:us-east-1:ACCOUNT:job-queue/cromwell-spot-queue"
          scriptBucketName = "cromwell-scripts"
        }
      }
    }
  }
}

# S3 for intermediate files
aws {
  application-name = "cromwell"
  auths = [{
    name = "default"
    scheme = "default"
  }]
}
EOF

# Create AMI
exit
spawn create-ami instance-id --name cromwell-orchestrator
```

**2. Run WDL Workflow:**

```bash
# Launch Cromwell orchestrator
spawn launch \
  --instance-type c6i.2xlarge \
  --ami ami-cromwell-orchestrator \
  --ttl 12h \
  --spot \
  --iam-policy batch:FullAccess,s3:FullAccess \
  --storage 100GB \
  --user-data "
    # Download workflow
    aws s3 cp s3://workflows/gatk-germline.wdl /workflows/pipeline.wdl
    aws s3 cp s3://workflows/inputs.json /workflows/inputs.json

    # Run Cromwell
    cd /workflows
    java -jar /usr/local/bin/cromwell.jar run pipeline.wdl --inputs inputs.json

    # Upload outputs
    aws s3 cp cromwell-executions/ s3://results/ --recursive

    # Signal completion
    spored complete --status success
  "
```

---

### GATK Germline Pipeline (WDL)

**Complete example:** `examples/genomics-wdl/gatk-germline/`

**Pipeline:** FASTQ → BAM → GVCF → VCF (30x WGS in ~4 hours)

**Launch:**
```bash
cd examples/genomics-wdl/gatk-germline/

# Edit inputs
cat > inputs.json <<EOF
{
  "GermlineVariantCalling.sample_name": "NA12878",
  "GermlineVariantCalling.fastq_r1": "s3://1000genomes/NA12878_R1.fastq.gz",
  "GermlineVariantCalling.fastq_r2": "s3://1000genomes/NA12878_R2.fastq.gz",
  "GermlineVariantCalling.reference_fasta": "s3://references/GRCh38.fa",
  "GermlineVariantCalling.known_sites_vcf": "s3://references/dbsnp_151.vcf.gz"
}
EOF

# Launch
spawn launch \
  --instance-type c6i.4xlarge \
  --ami ami-cromwell-orchestrator \
  --ttl 8h \
  --spot \
  --storage 500GB \
  --iam-policy s3:FullAccess,batch:FullAccess \
  --user-data "
    cd /opt/workflows
    java -jar /usr/local/bin/cromwell.jar run gatk-germline.wdl --inputs /tmp/inputs.json
    aws s3 cp cromwell-executions/ s3://results/NA12878/ --recursive
  "

# Cost: ~$0.50 (vs $10 on HealthOmics Ready2Run)
```

**Expected outputs:**
- `NA12878.bam` - Aligned, deduplicated BAM
- `NA12878.g.vcf.gz` - GVCF for joint calling
- `NA12878.vcf.gz` - Called variants
- Quality metrics

---

### RNA-seq Pipeline (WDL)

**Complete example:** `examples/genomics-wdl/rnaseq/`

**Pipeline:** FASTQ → BAM → Gene Counts (bulk RNA-seq)

**Launch:**
```bash
spawn launch \
  --params rnaseq-samples.yaml \
  --instance-type c6i.4xlarge \
  --array-size 50 \
  --spot \
  --ttl 4h \
  --storage 200GB
```

**rnaseq-samples.yaml:**
```yaml
parameters:
  sample_id: [TCGA-001, TCGA-002, ..., TCGA-050]
  fastq_r1: [s3://rnaseq/TCGA-001_R1.fq.gz, ...]
  fastq_r2: [s3://rnaseq/TCGA-001_R2.fq.gz, ...]
  genome: s3://references/GRCh38
  gtf: s3://references/gencode.v44.gtf

command: |
  # Run STAR alignment
  STAR --genomeDir ${genome} \
       --readFilesIn ${fastq_r1} ${fastq_r2} \
       --readFilesCommand zcat \
       --outSAMtype BAM SortedByCoordinate \
       --outFileNamePrefix ${sample_id}_

  # Count reads with featureCounts
  featureCounts -p -T 8 -a ${gtf} -o ${sample_id}_counts.txt ${sample_id}_Aligned.sortedByCoord.out.bam

  # Upload results
  aws s3 cp ${sample_id}_counts.txt s3://results/rnaseq/${sample_id}_counts.txt

instance-type: c6i.4xlarge
ami: ami-star-rnaseq
region: us-east-1
```

**Cost for 50 samples:** ~$25 (vs ~$183 on HealthOmics)

---

## CWL Workflows with cwltool

### Setup cwltool on spawn

**1. Create cwltool AMI:**

```bash
# Launch instance
spawn launch --instance-type c6i.2xlarge --wait-for-ssh

# Install cwltool
ssh instance
sudo yum install -y python3 docker
pip3 install cwltool

# Create AMI
exit
spawn create-ami instance-id --name cwltool-runner
```

**2. Run CWL Workflow:**

```bash
spawn launch \
  --instance-type c6i.4xlarge \
  --ami ami-cwltool-runner \
  --ttl 6h \
  --spot \
  --storage 300GB \
  --iam-policy s3:FullAccess \
  --user-data "
    # Download workflow
    aws s3 cp s3://workflows/variant-calling.cwl /workflows/pipeline.cwl
    aws s3 cp s3://workflows/inputs.yml /workflows/inputs.yml

    # Run cwltool
    cd /workflows
    cwltool --outdir /results pipeline.cwl inputs.yml

    # Upload results
    aws s3 sync /results s3://results/
  "
```

---

### Variant Calling Pipeline (CWL)

**Complete example:** `examples/genomics-cwl/variant-calling/`

**Pipeline:** BAM → VCF (germline variant calling)

**variant-calling.cwl:**
```yaml
cwlVersion: v1.2
class: Workflow

inputs:
  input_bam:
    type: File
    secondaryFiles:
      - .bai
  reference_fasta:
    type: File
    secondaryFiles:
      - .fai
      - ^.dict
  sample_name: string

outputs:
  output_vcf:
    type: File
    outputSource: filter_variants/filtered_vcf

steps:
  haplotype_caller:
    run: gatk-haplotypecaller.cwl
    in:
      input_bam: input_bam
      reference: reference_fasta
      sample_name: sample_name
    out: [raw_vcf]

  filter_variants:
    run: gatk-variantfiltration.cwl
    in:
      input_vcf: haplotype_caller/raw_vcf
      reference: reference_fasta
    out: [filtered_vcf]
```

**Launch:**
```bash
cd examples/genomics-cwl/variant-calling/

spawn launch \
  --instance-type c6i.4xlarge \
  --ami ami-cwltool-runner \
  --ttl 4h \
  --spot \
  --storage 200GB \
  --user-data "
    cwltool --outdir /results variant-calling.cwl inputs.yml
    aws s3 sync /results s3://results/$(date +%Y%m%d)/
  "
```

---

## Nextflow with nf-core Pipelines

### Setup Nextflow on spawn

**1. Create Nextflow AMI:**

```bash
# Launch instance
spawn launch --instance-type c6i.2xlarge --wait-for-ssh

# Install Nextflow
ssh instance
sudo yum install -y java-11-amazon-corretto docker
curl -s https://get.nextflow.io | bash
sudo mv nextflow /usr/local/bin/
sudo chmod +x /usr/local/bin/nextflow

# Install nf-core tools
pip3 install nf-core

# Create AMI
exit
spawn create-ami instance-id --name nextflow-nfcore
```

**2. Configure Nextflow for AWS:**

**nextflow.config:**
```groovy
// AWS configuration
aws {
    region = 'us-east-1'
    batch {
        cliPath = '/usr/local/aws-cli/v2/current/bin/aws'
        maxParallelTransfers = 10
        maxTransferAttempts = 3
    }
}

// Use spot instances for cost savings
process {
    executor = 'awsbatch'
    queue = 'nextflow-spot-queue'
    container = 'nfcore/base:latest'

    // Resource labels for different process types
    withLabel: 'process_low' {
        cpus = 2
        memory = '8 GB'
    }
    withLabel: 'process_medium' {
        cpus = 8
        memory = '32 GB'
    }
    withLabel: 'process_high' {
        cpus = 16
        memory = '64 GB'
    }
}

// S3 work directory
workDir = 's3://nextflow-work/'
```

---

### nf-core/sarek (Variant Calling)

**Complete example:** `examples/genomics-nextflow/nf-core-sarek/`

**Pipeline:** FASTQ → VCF (germline + somatic variants, CNV, SV)

**Launch:**
```bash
spawn launch \
  --instance-type c6i.2xlarge \
  --ami ami-nextflow-nfcore \
  --ttl 12h \
  --spot \
  --storage 100GB \
  --iam-policy batch:FullAccess,s3:FullAccess \
  --user-data "
    # Create sample sheet
    cat > samplesheet.csv <<EOF
patient,sample,lane,fastq_1,fastq_2
NA12878,NA12878,L001,s3://data/NA12878_R1.fastq.gz,s3://data/NA12878_R2.fastq.gz
EOF

    # Run nf-core/sarek
    nextflow run nf-core/sarek \
      -profile docker,awsbatch \
      --input samplesheet.csv \
      --genome GRCh38 \
      --outdir s3://results/sarek/ \
      --tools haplotypecaller,mutect2,strelka \
      -work-dir s3://nextflow-work/sarek/ \
      -resume

    # Signal completion
    spored complete --status success
  "
```

**Expected runtime:** 6-8 hours for 30x WGS
**Cost:** ~$1-2 on spawn spot (vs $10+ on HealthOmics)

---

### nf-core/rnaseq (RNA-seq Analysis)

**Complete example:** `examples/genomics-nextflow/nf-core-rnaseq/`

**Pipeline:** FASTQ → Gene Counts + QC

**Launch with spawn array:**
```bash
# Create sample sheet for 100 samples
cat > rnaseq-samples.yaml <<EOF
parameters:
  sample_sheet: [sample1.csv, sample2.csv, ..., sample100.csv]

command: |
  nextflow run nf-core/rnaseq \
    -profile docker \
    --input \${sample_sheet} \
    --genome GRCh38 \
    --outdir s3://results/rnaseq/\${TASK_ARRAY_INDEX}/ \
    -work-dir /tmp/work/

instance-type: c6i.4xlarge
ami: ami-nextflow-nfcore
region: us-east-1
storage: 200GB
EOF

spawn launch --params rnaseq-samples.yaml --array-size 100 --spot --ttl 6h
```

**Cost for 100 samples:** ~$50 (vs ~$367 on HealthOmics)

---

## Migrating from AWS HealthOmics

### Decision Matrix

| Use Case | Use HealthOmics | Use spawn |
|----------|----------------|-----------|
| Clinical diagnostics (HIPAA/CLIA) | ✅ Yes | Consider (compliance DIY) |
| Production pipelines (audit trails) | ✅ Yes | Consider (manual auditing) |
| Research workflows (cost-sensitive) | ❌ No | ✅ **Yes** (7x cheaper) |
| Development/testing | ❌ No | ✅ **Yes** (iteration cost) |
| Snakemake workflows | ❌ Not supported | ✅ **Yes** (only option) |
| Custom Python pipelines | ❌ Not supported | ✅ **Yes** (full flexibility) |
| Population genomics (>10K samples) | Consider | ✅ **Yes** (cost scales) |

---

### Migration Steps

**1. Identify Workflow Language:**
- HealthOmics WDL → spawn with Cromwell/miniwdl
- HealthOmics Nextflow → spawn with Nextflow (direct migration)
- HealthOmics CWL → spawn with cwltool/Toil

**2. Test Workflow on spawn:**
```bash
# Launch test run
spawn launch \
  --instance-type c6i.2xlarge \
  --ami ami-your-orchestrator \
  --ttl 4h \
  --spot \
  --user-data "run-workflow-test.sh"
```

**3. Compare Costs:**
```bash
# After test completes
spawn status instance-id --json | jq '.cost'
# Compare to HealthOmics invoice
```

**4. Migrate Production Workflows:**
- Update CI/CD to use spawn instead of HealthOmics API
- Configure spawn job arrays for parallel samples
- Set up S3 buckets for results

**5. Implement Audit Logging (if needed):**
```bash
# Enable CloudTrail for compliance
# Log all spawn launches to DynamoDB
# Track workflow provenance manually
```

---

## Cost Comparison: Real Examples

### Example 1: Whole Genome Germline (30x)

| Solution | Cost/Sample | Cost/1000 Samples | Time |
|----------|-------------|-------------------|------|
| HealthOmics Ready2Run | $10.00 | $10,000 | 4h |
| HealthOmics Private | $3.67 | $3,670 | 4h |
| **spawn (Cromwell + WDL)** | **$0.50** | **$500** | 4h |
| spawn (Nextflow + sarek) | $1.20 | $1,200 | 6h |

**spawn savings:** $9,500 per 1000 samples (95% cheaper than Ready2Run)

---

### Example 2: RNA-seq (bulk, 50M reads)

| Solution | Cost/Sample | Cost/100 Samples | Time |
|----------|-------------|------------------|------|
| HealthOmics Private | $1.83 | $183 | 2h |
| **spawn (nf-core/rnaseq)** | **$0.25** | **$25** | 2h |

**spawn savings:** $158 per 100 samples (86% cheaper)

---

### Example 3: Targeted Panel Sequencing

| Solution | Cost/Sample | Cost/10K Samples | Time |
|----------|-------------|------------------|------|
| HealthOmics Private | $0.50 | $5,000 | 30min |
| **spawn (custom pipeline)** | **$0.08** | **$800** | 30min |

**spawn savings:** $4,200 per 10K samples (84% cheaper)

---

## Best Practices

### 1. Use Spot Instances
```bash
# Always use spot for cost savings
spawn launch --spot ...
# HealthOmics doesn't offer spot pricing
```

### 2. Pre-build AMIs
```bash
# Build once, launch fast
spawn create-ami orchestrator-instance --name cromwell-v85

# Reuse for all workflows
spawn launch --ami ami-cromwell-v85 ...
```

### 3. Cache Reference Genomes
```bash
# Download once to EBS snapshot
# Attach to all instances
spawn launch --snapshot snap-references ...
```

### 4. Use spawn Arrays for Parallel Samples
```bash
# Launch 100 samples in parallel
spawn launch --params samples.yaml --array-size 100 --spot
```

### 5. Set Aggressive TTLs
```bash
# Auto-terminate on completion
spawn launch --ttl 8h --on-complete terminate
```

### 6. Monitor Costs
```bash
# Track spending
spawn status --all --json | jq '[.[] | .cost] | add'
```

---

## Troubleshooting

### Workflow Fails on spawn but Works on HealthOmics

**Problem:** Workflow runs on HealthOmics but fails on spawn.

**Common Causes:**
1. Missing Docker images
2. Insufficient instance resources
3. IAM permissions not configured

**Solution:**
```bash
# Check logs
spawn logs instance-id

# Increase resources
spawn launch --instance-type c6i.8xlarge --storage 500GB ...

# Fix IAM permissions
spawn launch --iam-policy s3:FullAccess,batch:FullAccess ...
```

---

### Out of Disk Space

**Problem:** Workflow fails with disk space errors.

**Solution:**
```bash
# Increase storage
spawn launch --storage 1000GB ...

# Or use /tmp with instance store
spawn launch --instance-type c6id.4xlarge ...  # 'd' = NVMe instance store
```

---

### Workflow Hangs

**Problem:** Workflow doesn't complete.

**Cause:** Often waiting for AWS Batch queue or insufficient permissions.

**Solution:**
```bash
# Check if using AWS Batch
# Ensure Batch queue has capacity

# Or run everything on single instance
# No Batch dependency
```

---

## Advanced: spawn as HealthOmics Drop-in Replacement

### spawn API Wrapper

For teams with existing HealthOmics API calls, create spawn wrapper:

**spawn-healthomics-compat.py:**
```python
#!/usr/bin/env python3
"""spawn compatibility wrapper for HealthOmics API"""

import boto3
import subprocess
import json

def start_run(workflow_id, parameters):
    """
    Replace HealthOmics StartRun with spawn launch
    """
    # Convert HealthOmics parameters to spawn format
    spawn_params = convert_parameters(parameters)

    # Launch with spawn
    result = subprocess.run([
        'spawn', 'launch',
        '--params', spawn_params,
        '--format', 'json'
    ], capture_output=True)

    # Return HealthOmics-compatible response
    return {
        'id': result.stdout.decode().strip(),
        'arn': f'arn:aws:spawn:us-east-1:123456789:run/{result.stdout}',
        'status': 'RUNNING'
    }

def get_run(run_id):
    """
    Replace HealthOmics GetRun with spawn status
    """
    result = subprocess.run([
        'spawn', 'status', run_id, '--format', 'json'
    ], capture_output=True)

    status = json.loads(result.stdout)

    # Map to HealthOmics format
    return {
        'id': run_id,
        'status': map_status(status['Status']),
        'startTime': status.get('LaunchTime'),
        'outputUri': f"s3://results/{run_id}/"
    }

# Use in existing code:
# omics = boto3.client('omics')
# run = omics.start_run(...)
#
# Replace with:
# run = start_run(workflow_id, parameters)
```

---

## Summary

**spawn provides HealthOmics capabilities at 7x lower cost:**

| Feature | spawn | HealthOmics |
|---------|-------|-------------|
| **Cost** | $0.50/WGS | $3.67-$10/WGS |
| **Workflow Languages** | WDL, CWL, Nextflow, Snakemake, custom | WDL, CWL, Nextflow only |
| **Management** | User-managed | Fully managed |
| **Flexibility** | Full control | Limited |
| **Vendor Lock-in** | None | High |

**Use spawn for:**
- Research workflows (cost-sensitive)
- Development/testing (rapid iteration)
- Snakemake pipelines (not supported by HealthOmics)
- Custom pipelines (full flexibility)
- Population-scale studies (costs scale linearly)

**Use HealthOmics for:**
- Clinical diagnostics (HIPAA/CLIA compliance built-in)
- Zero DevOps capacity (fully managed)
- Budget allows 7x premium for convenience

---

## See Also

- [Complete WDL Examples](../../examples/genomics-wdl/)
- [Complete CWL Examples](../../examples/genomics-cwl/)
- [Complete Nextflow Examples](../../examples/genomics-nextflow/)
- [AWS HealthOmics Research](../../research/aws-healthomics.md)
- [Cost Optimization](cost-optimization.md)
- [Spot Instances](spot-instances.md)
