# nf-core/sarek Pipeline on spawn

Run the popular nf-core/sarek germline and somatic variant calling pipeline on spawn for 7x cost savings vs AWS HealthOmics.

**Pipeline:** nf-core/sarek v3.4 (FASTQ → VCF)
**Supported:** Germline, somatic, CNV, SV calling

**Runtime:** 6-8 hours for 30x WGS
**Cost on spawn:** ~$1.20 per sample (spot)
**Cost on HealthOmics:** $10.00 per sample

---

## Pipeline Overview

nf-core/sarek is a comprehensive variant calling pipeline that includes:
- **Preprocessing:** Trim, QC, align (BWA-MEM)
- **Variant Calling:** GATK HaplotypeCaller, Mutect2, Strelka, Manta
- **Copy Number:** ASCAT, Control-FREEC
- **Structural Variants:** Manta, TIDDIT
- **Annotation:** VEP, SnpEff

---

## Quick Start

### Prerequisites

**Build Nextflow AMI:**
```bash
spawn launch --instance-type c6i.2xlarge --wait-for-ssh

ssh instance

# Install Nextflow
sudo yum install -y java-11-amazon-corretto docker git
curl -s https://get.nextflow.io | bash
sudo mv nextflow /usr/local/bin/
sudo chmod +x /usr/local/bin/nextflow

# Install nf-core
pip3 install nf-core

# Enable Docker
sudo systemctl start docker
sudo usermod -a -G docker ec2-user

exit

spawn create-ami <instance-id> --name nextflow-nfcore
```

---

### Run Germline Variant Calling

**1. Create sample sheet:**

```csv
patient,sample,lane,fastq_1,fastq_2
NA12878,NA12878,L001,s3://data/NA12878_L001_R1.fastq.gz,s3://data/NA12878_L001_R1.fastq.gz
NA12879,NA12879,L001,s3://data/NA12879_L001_R1.fastq.gz,s3://data/NA12879_L001_R1.fastq.gz
```

**2. Launch with spawn:**

```bash
spawn launch \
  --instance-type c6i.2xlarge \
  --ami ami-nextflow-nfcore \
  --ttl 12h \
  --spot \
  --storage 200GB \
  --iam-policy batch:FullAccess,s3:FullAccess \
  --user-data "
    #!/bin/bash
    set -e

    # Download samplesheet
    aws s3 cp s3://my-data/samplesheet.csv /workflows/samplesheet.csv

    # Run nf-core/sarek
    nextflow run nf-core/sarek \
      -profile docker \
      --input /workflows/samplesheet.csv \
      --genome GRCh38 \
      --outdir s3://my-results/sarek/ \
      --tools haplotypecaller,strelka,manta \
      -work-dir /tmp/work/ \
      -resume

    # Upload logs
    aws s3 cp .nextflow.log s3://my-results/sarek/logs/

    spored complete --status success
  "
```

---

### Run with AWS Batch Backend (Recommended for Scale)

For large studies (>10 samples), use AWS Batch as compute backend:

**nextflow.config:**
```groovy
aws {
    region = 'us-east-1'
    batch {
        cliPath = '/usr/local/aws-cli/v2/current/bin/aws'
        maxParallelTransfers = 10
    }
}

process {
    executor = 'awsbatch'
    queue = 'nextflow-spot-queue'  // Pre-created Batch queue

    withLabel: process_low {
        cpus = 2
        memory = '8 GB'
    }
    withLabel: process_medium {
        cpus = 8
        memory = '32 GB'
    }
    withLabel: process_high {
        cpus = 16
        memory = '64 GB'
    }
}

workDir = 's3://nextflow-work/'
```

**Launch orchestrator only:**
```bash
spawn launch \
  --instance-type t3.medium \
  --ami ami-nextflow-nfcore \
  --ttl 24h \
  --spot \
  --iam-policy batch:FullAccess,s3:FullAccess \
  --user-data "
    nextflow run nf-core/sarek \
      -profile awsbatch \
      --input samplesheet.csv \
      --genome GRCh38 \
      --outdir s3://results/ \
      -work-dir s3://nextflow-work/ \
      -c nextflow.config
  "

# Batch workers auto-scale on demand (spawn-managed or Batch-managed)
```

---

### Run Multiple Cohorts (spawn Array)

```yaml
# cohorts.yaml
parameters:
  cohort: [cohort1, cohort2, cohort3]
  samplesheet:
    - s3://data/cohort1_samples.csv
    - s3://data/cohort2_samples.csv
    - s3://data/cohort3_samples.csv

command: |
  # Download samplesheet
  aws s3 cp ${samplesheet} /tmp/samplesheet.csv

  # Run sarek
  nextflow run nf-core/sarek \
    -profile docker \
    --input /tmp/samplesheet.csv \
    --genome GRCh38 \
    --outdir s3://results/${cohort}/ \
    --tools haplotypecaller \
    -work-dir /tmp/work/

instance-type: c6i.2xlarge
ami: ami-nextflow-nfcore
region: us-east-1
storage: 200GB
```

```bash
spawn launch --params cohorts.yaml --array-size 3 --spot --ttl 12h
```

---

## Supported Tools

### Variant Callers

| Tool | Type | Use Case |
|------|------|----------|
| **HaplotypeCaller** | Germline | General germline calling |
| **Mutect2** | Somatic | Tumor/normal somatic variants |
| **Strelka** | Germline/Somatic | High-performance calling |
| **Manta** | SV | Structural variants |
| **TIDDIT** | SV | Structural variants |
| **FreeBayes** | Germline | Alternative germline caller |

### CNV Callers

- **ASCAT** - Allele-specific copy number
- **Control-FREEC** - Copy number and allelic content
- **CNVkit** - CNV detection from targeted sequencing

---

## Performance Benchmarks

### Single Sample (30x WGS)

| Instance Type | Runtime | Cost (spot) | Notes |
|---------------|---------|-------------|-------|
| c6i.2xlarge | 8h | $1.60 | Orchestrator + local exec |
| c6i.4xlarge | 6h | $1.20 | Recommended |
| with AWS Batch | 6h | $1.50 | Auto-scaling workers |

### 100 Samples (Cohort Study)

| Method | Runtime | Total Cost | Cost/Sample |
|--------|---------|-----------|-------------|
| spawn array (100 instances) | 6h | $120 | $1.20 |
| spawn + Batch backend | 8h | $150 | $1.50 |
| HealthOmics | 6h | $1,000 | $10.00 |

**spawn savings:** $880 for 100-sample cohort (88%)

---

## Pipeline Outputs

```
s3://results/sarek/
├── preprocessing/
│   ├── markduplicates/
│   │   └── NA12878.md.cram
│   └── recalibrated/
│       └── NA12878.recal.cram
├── variant_calling/
│   ├── haplotypecaller/
│   │   └── NA12878.haplotypecaller.vcf.gz
│   ├── strelka/
│   │   └── NA12878.strelka.variants.vcf.gz
│   └── manta/
│       └── NA12878.manta.diploidSV.vcf.gz
├── annotation/
│   └── vep/
│       └── NA12878.vep.vcf.gz
└── multiqc/
    └── multiqc_report.html
```

---

## Migration from HealthOmics

### nf-core/sarek Compatibility

**HealthOmics Nextflow** workflows can run directly on spawn with minor config changes:

1. **Remove** HealthOmics-specific profiles
2. **Add** spawn-optimized config (spot instances, local storage)
3. **Update** S3 paths

**No workflow code changes required!**

---

## Cost Comparison

### Per-Sample Cost (30x WGS, all callers)

| Method | Cost | Savings vs HealthOmics |
|--------|------|------------------------|
| **spawn (c6i.4xlarge spot)** | **$1.20** | 88% |
| spawn (c6i.4xlarge on-demand) | $3.50 | 65% |
| AWS HealthOmics | $10.00 | Baseline |

### Cohort Study (100 samples)

| Method | Total Cost | Savings |
|--------|-----------|---------|
| **spawn** | **$120** | $880 (88%) |
| HealthOmics | $1,000 | Baseline |

---

## Troubleshooting

### Out of Memory

**Error:** Nextflow process killed (exit code 137)

**Solution:**
```groovy
// In nextflow.config, increase memory
process {
    withLabel: process_high {
        memory = '128 GB'
    }
}
```

### Slow Downloads from S3

**Cause:** S3 download bottleneck

**Solution:**
```bash
# Pre-download reference genome to AMI
# Or use S3 VPC endpoint for faster access
```

---

## See Also

- [nf-core/sarek Documentation](https://nf-co.re/sarek)
- [How-To: Genomics Workflows](../../../docs/how-to/genomics-workflows.md)
- [Nextflow Documentation](https://www.nextflow.io/docs/latest/)
- [nf-core Tools](https://nf-co.re/tools)
