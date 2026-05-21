# GATK Germline Variant Calling Pipeline (WDL)

Complete GATK Best Practices germline variant calling pipeline for spawn.

**Pipeline:** FASTQ → Aligned BAM → GVCF → VCF

**Runtime:** ~4 hours for 30x WGS on c6i.4xlarge
**Cost on spawn:** ~$0.50 per sample (spot instance)
**Cost on HealthOmics:** $10.00 per sample (Ready2Run)

---

## Pipeline Overview

```
Input: Paired-end FASTQ files (FASTQ.gz)
  ↓
1. FastQC - Quality control
  ↓
2. BWA-MEM - Align to reference (GRCh38)
  ↓
3. MarkDuplicates - Remove PCR duplicates
  ↓
4. Base Quality Score Recalibration (BQSR)
  ↓
5. HaplotypeCaller - Call variants (GVCF mode)
  ↓
6. GenotypeGVCFs - Joint genotyping
  ↓
7. VariantFiltration - Apply quality filters
  ↓
Output: Filtered VCF
```

---

## Prerequisites

### 1. Build Cromwell AMI

```bash
# Launch base instance
spawn launch \
  --instance-type c6i.2xlarge \
  --ami ami-amazon-linux-2023 \
  --wait-for-ssh \
  --storage 100GB

# SSH and install
ssh instance

# Install Java and Cromwell
sudo yum install -y java-11-amazon-corretto docker git
wget https://github.com/broadinstitute/cromwell/releases/download/85/cromwell-85.jar
sudo mv cromwell-85.jar /usr/local/bin/cromwell.jar
sudo chmod +x /usr/local/bin/cromwell.jar

# Install GATK
wget https://github.com/broadinstitute/gatk/releases/download/4.5.0.0/gatk-4.5.0.0.zip
unzip gatk-4.5.0.0.zip
sudo mv gatk-4.5.0.0 /opt/gatk
sudo ln -s /opt/gatk/gatk /usr/local/bin/gatk

# Install BWA
sudo yum install -y bwa samtools

# Install FastQC
sudo yum install -y fastqc

# Create Cromwell config
sudo mkdir -p /etc/cromwell
sudo cat > /etc/cromwell/cromwell.conf <<'EOF'
include required(classpath("application"))

backend {
  default = "Local"
  providers {
    Local {
      actor-factory = "cromwell.backend.impl.sfs.config.ConfigBackendLifecycleActorFactory"
      config {
        root = "/cromwell-executions"

        runtime-attributes = """
          Int cpu = 1
          Float memory_gb = 4.0
          String docker = ""
        """

        submit = "/bin/bash ${script}"
        submit-docker = "docker run --rm -v ${cwd}:${docker_cwd} -i ${docker} /bin/bash < ${script}"
      }
    }
  }
}

system {
  io {
    number-of-requests = 100000
    per = 100 seconds
  }
}
EOF

# Enable Docker
sudo systemctl start docker
sudo systemctl enable docker
sudo usermod -a -G docker ec2-user

# Exit and create AMI
exit

# Create AMI
spawn create-ami <instance-id> --name cromwell-gatk-v85
```

### 2. Download Reference Files

```bash
# Create S3 bucket for references
aws s3 mb s3://my-genomics-references

# Download GRCh38 reference
wget https://storage.googleapis.com/genomics-public-data/resources/broad/hg38/v0/Homo_sapiens_assembly38.fasta
aws s3 cp Homo_sapiens_assembly38.fasta s3://my-genomics-references/GRCh38/

# Index files
wget https://storage.googleapis.com/genomics-public-data/resources/broad/hg38/v0/Homo_sapiens_assembly38.fasta.fai
wget https://storage.googleapis.com/genomics-public-data/resources/broad/hg38/v0/Homo_sapiens_assembly38.dict
aws s3 cp Homo_sapiens_assembly38.fasta.fai s3://my-genomics-references/GRCh38/
aws s3 cp Homo_sapiens_assembly38.dict s3://my-genomics-references/GRCh38/

# Known sites for BQSR
wget https://storage.googleapis.com/genomics-public-data/resources/broad/hg38/v0/Homo_sapiens_assembly38.dbsnp138.vcf
wget https://storage.googleapis.com/genomics-public-data/resources/broad/hg38/v0/Mills_and_1000G_gold_standard.indels.hg38.vcf.gz
aws s3 cp Homo_sapiens_assembly38.dbsnp138.vcf s3://my-genomics-references/GRCh38/
aws s3 cp Mills_and_1000G_gold_standard.indels.hg38.vcf.gz s3://my-genomics-references/GRCh38/
```

---

## Quick Start

### Single Sample

**1. Create inputs file:**

```json
{
  "GatkGermline.sample_name": "NA12878",
  "GatkGermline.fastq_r1": "s3://1000genomes/NA12878_R1.fastq.gz",
  "GatkGermline.fastq_r2": "s3://1000genomes/NA12878_R2.fastq.gz",
  "GatkGermline.reference_fasta": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.fasta",
  "GatkGermline.reference_fai": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.fasta.fai",
  "GatkGermline.reference_dict": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.dict",
  "GatkGermline.dbsnp_vcf": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.dbsnp138.vcf",
  "GatkGermline.known_indels_vcf": "s3://my-genomics-references/GRCh38/Mills_and_1000G_gold_standard.indels.hg38.vcf.gz"
}
```

**2. Launch with spawn:**

```bash
# Upload inputs
aws s3 cp inputs.json s3://my-workflows/inputs.json

# Launch orchestrator
spawn launch \
  --instance-type c6i.4xlarge \
  --ami ami-cromwell-gatk-v85 \
  --ttl 8h \
  --spot \
  --storage 500GB \
  --iam-policy s3:FullAccess \
  --tags pipeline=gatk-germline,sample=NA12878 \
  --user-data "
    #!/bin/bash
    set -e

    # Download workflow and inputs
    aws s3 cp s3://my-workflows/gatk-germline.wdl /workflows/pipeline.wdl
    aws s3 cp s3://my-workflows/inputs.json /workflows/inputs.json

    # Download reference files to local disk for faster access
    mkdir -p /references
    aws s3 cp s3://my-genomics-references/GRCh38/ /references/ --recursive

    # Run Cromwell
    cd /workflows
    java -Xmx8g -jar /usr/local/bin/cromwell.jar run pipeline.wdl --inputs inputs.json

    # Upload outputs
    aws s3 cp cromwell-executions/ s3://my-results/NA12878/ --recursive

    # Signal completion
    spored complete --status success
  "
```

**3. Monitor progress:**

```bash
# Check status
spawn status <instance-id>

# View logs
spawn logs <instance-id>

# Download results (after completion)
aws s3 sync s3://my-results/NA12878/ ./results/
```

---

### Multiple Samples (spawn Array)

**1. Create sample manifest:**

```yaml
# samples.yaml
parameters:
  sample_name: [NA12878, NA12879, NA12880, NA12881]
  fastq_r1:
    - s3://data/NA12878_R1.fastq.gz
    - s3://data/NA12879_R1.fastq.gz
    - s3://data/NA12880_R1.fastq.gz
    - s3://data/NA12881_R1.fastq.gz
  fastq_r2:
    - s3://data/NA12878_R2.fastq.gz
    - s3://data/NA12879_R2.fastq.gz
    - s3://data/NA12880_R2.fastq.gz
    - s3://data/NA12881_R2.fastq.gz

command: |
  # Create inputs.json for this sample
  cat > /tmp/inputs.json <<EOF
  {
    "GatkGermline.sample_name": "${sample_name}",
    "GatkGermline.fastq_r1": "${fastq_r1}",
    "GatkGermline.fastq_r2": "${fastq_r2}",
    "GatkGermline.reference_fasta": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.fasta",
    "GatkGermline.reference_fai": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.fasta.fai",
    "GatkGermline.reference_dict": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.dict",
    "GatkGermline.dbsnp_vcf": "s3://my-genomics-references/GRCh38/Homo_sapiens_assembly38.dbsnp138.vcf",
    "GatkGermline.known_indels_vcf": "s3://my-genomics-references/GRCh38/Mills_and_1000G_gold_standard.indels.hg38.vcf.gz"
  }
  EOF

  # Run workflow
  cd /workflows
  java -Xmx8g -jar /usr/local/bin/cromwell.jar run gatk-germline.wdl --inputs /tmp/inputs.json

  # Upload results
  aws s3 cp cromwell-executions/ s3://my-results/${sample_name}/ --recursive

instance-type: c6i.4xlarge
ami: ami-cromwell-gatk-v85
region: us-east-1
storage: 500GB
```

**2. Launch array:**

```bash
spawn launch \
  --params samples.yaml \
  --array-size 4 \
  --spot \
  --ttl 8h

# Cost: 4 samples × $0.50 = $2.00 (vs $40 on HealthOmics)
```

---

## Pipeline Outputs

```
cromwell-executions/GatkGermline/<workflow-id>/call-*/
├── fastqc_reports/
│   ├── NA12878_R1_fastqc.html
│   └── NA12878_R2_fastqc.html
├── aligned/
│   ├── NA12878.bam
│   └── NA12878.bam.bai
├── markduplicates/
│   ├── NA12878.dedup.bam
│   ├── NA12878.dedup.bam.bai
│   └── NA12878.dedup.metrics.txt
├── bqsr/
│   ├── NA12878.recal.bam
│   ├── NA12878.recal.bam.bai
│   └── NA12878.recal.table
├── haplotypecaller/
│   ├── NA12878.g.vcf.gz
│   └── NA12878.g.vcf.gz.tbi
└── genotype_gvcfs/
    ├── NA12878.vcf.gz
    ├── NA12878.vcf.gz.tbi
    └── NA12878.filtered.vcf.gz
```

---

## Performance Benchmarks

| Instance Type | vCPU | Memory | Storage | Runtime | Cost (spot) |
|---------------|------|--------|---------|---------|-------------|
| c6i.2xlarge | 8 | 16 GB | 500 GB EBS | 8h | $0.70 |
| c6i.4xlarge | 16 | 32 GB | 500 GB EBS | 4h | $0.50 |
| c6i.8xlarge | 32 | 64 GB | 1 TB EBS | 2.5h | $0.60 |
| c6i.16xlarge | 64 | 128 GB | 1 TB EBS | 1.5h | $0.90 |

**Recommended:** c6i.4xlarge (best cost/performance balance)

---

## Troubleshooting

### Out of Memory

**Error:** `java.lang.OutOfMemoryError`

**Solution:**
```bash
# Increase Java heap
java -Xmx16g -jar /usr/local/bin/cromwell.jar run pipeline.wdl
```

### Out of Disk Space

**Error:** `No space left on device`

**Solution:**
```bash
# Increase storage or use instance with NVMe
spawn launch --storage 1000GB ...
# Or
spawn launch --instance-type c6id.4xlarge ...  # 'd' = NVMe instance store
```

### BWA Alignment Slow

**Cause:** Not enough threads allocated

**Solution:**
```wdl
# In WDL file, increase runtime.cpu
runtime {
  cpu: 16  # Use all available cores
  memory: "32 GB"
}
```

---

## Cost Comparison

### Per-Sample Cost (30x WGS)

| Method | Runtime | Cost | Notes |
|--------|---------|------|-------|
| **spawn (c6i.4xlarge spot)** | 4h | **$0.50** | This pipeline |
| spawn (c6i.4xlarge on-demand) | 4h | $1.40 | No spot savings |
| AWS HealthOmics Private | 4h | $3.67 | Managed service |
| AWS HealthOmics Ready2Run | 4h | $10.00 | Pre-built pipeline |

**spawn savings vs HealthOmics:** $9.50 per sample (95%)

### At Scale (1000 samples)

| Method | Total Cost |
|--------|-----------|
| spawn (spot) | **$500** |
| spawn (on-demand) | $1,400 |
| HealthOmics Private | $3,670 |
| HealthOmics Ready2Run | $10,000 |

**spawn savings at scale:** $9,500 (vs Ready2Run)

---

## See Also

- [gatk-germline.wdl](gatk-germline.wdl) - Complete WDL workflow
- [How-To: Genomics Workflows](../../../docs/how-to/genomics-workflows.md)
- [GATK Best Practices](https://gatk.broadinstitute.org/hc/en-us/articles/360035535932-Germline-short-variant-discovery-SNPs-Indels-)
- [Cromwell Documentation](https://cromwell.readthedocs.io/)
