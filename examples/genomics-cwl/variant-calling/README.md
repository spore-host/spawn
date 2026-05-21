# Variant Calling Pipeline (CWL)

Complete germline variant calling pipeline using Common Workflow Language (CWL) on spawn.

**Pipeline:** BAM → VCF (GATK HaplotypeCaller + filtration)

**Runtime:** ~2 hours for 30x WGS BAM on c6i.4xlarge
**Cost on spawn:** ~$0.25 per sample (spot instance)
**Cost on HealthOmics:** $2.00 per sample

---

## Pipeline Overview

```
Input: Aligned BAM file
  ↓
1. HaplotypeCaller - Call variants
  ↓
2. VariantFiltration - Apply quality filters
  ↓
Output: Filtered VCF
```

---

## Quick Start

### Prerequisites

**Install cwltool:**
```bash
pip3 install cwltool
```

**Build CWL-ready AMI:**
```bash
spawn launch --instance-type c6i.2xlarge --wait-for-ssh

ssh instance
sudo yum install -y python3 docker
pip3 install cwltool

# Enable Docker
sudo systemctl start docker
sudo usermod -a -G docker ec2-user

exit
spawn create-ami <instance-id> --name cwltool-runner
```

---

### Run Single Sample

**1. Create inputs file:**

```yaml
# inputs.yml
input_bam:
  class: File
  path: s3://my-data/NA12878.bam
  secondaryFiles:
    - class: File
      path: s3://my-data/NA12878.bam.bai

reference_fasta:
  class: File
  path: s3://references/GRCh38/Homo_sapiens_assembly38.fasta
  secondaryFiles:
    - class: File
      path: s3://references/GRCh38/Homo_sapiens_assembly38.fasta.fai
    - class: File
      path: s3://references/GRCh38/Homo_sapiens_assembly38.dict

dbsnp_vcf:
  class: File
  path: s3://references/GRCh38/Homo_sapiens_assembly38.dbsnp138.vcf
  secondaryFiles:
    - class: File
      path: s3://references/GRCh38/Homo_sapiens_assembly38.dbsnp138.vcf.idx

sample_name: "NA12878"
```

**2. Launch with spawn:**

```bash
spawn launch \
  --instance-type c6i.4xlarge \
  --ami ami-cwltool-runner \
  --ttl 4h \
  --spot \
  --storage 300GB \
  --iam-policy s3:FullAccess \
  --user-data "
    #!/bin/bash
    set -e

    # Download workflow
    aws s3 cp s3://my-workflows/variant-calling.cwl /workflows/pipeline.cwl
    aws s3 cp s3://my-workflows/inputs.yml /workflows/inputs.yml

    # Run cwltool
    cd /workflows
    cwltool --outdir /results pipeline.cwl inputs.yml

    # Upload results
    aws s3 sync /results s3://my-results/NA12878/

    spored complete --status success
  "
```

---

### Run Multiple Samples (spawn Array)

```yaml
# samples.yaml
parameters:
  sample_name: [NA12878, NA12879, NA12880]
  bam_file:
    - s3://data/NA12878.bam
    - s3://data/NA12879.bam
    - s3://data/NA12880.bam

command: |
  # Create inputs for this sample
  cat > /tmp/inputs.yml <<EOF
  input_bam:
    class: File
    path: ${bam_file}
    secondaryFiles:
      - class: File
        path: ${bam_file}.bai

  reference_fasta:
    class: File
    path: s3://references/GRCh38/Homo_sapiens_assembly38.fasta
    secondaryFiles:
      - class: File
        path: s3://references/GRCh38/Homo_sapiens_assembly38.fasta.fai

  sample_name: "${sample_name}"
  EOF

  # Run workflow
  cwltool --outdir /results variant-calling.cwl /tmp/inputs.yml

  # Upload
  aws s3 sync /results s3://my-results/${sample_name}/

instance-type: c6i.4xlarge
ami: ami-cwltool-runner
region: us-east-1
storage: 300GB
```

```bash
spawn launch --params samples.yaml --array-size 3 --spot --ttl 4h
```

---

## Outputs

```
/results/
├── NA12878.vcf.gz
├── NA12878.vcf.gz.tbi
└── NA12878.filtered.vcf.gz
```

---

## Performance

| Instance Type | Runtime | Cost (spot) |
|---------------|---------|-------------|
| c6i.2xlarge | 4h | $0.35 |
| c6i.4xlarge | 2h | $0.25 |
| c6i.8xlarge | 1h | $0.30 |

**Recommended:** c6i.4xlarge

---

## See Also

- [variant-calling.cwl](variant-calling.cwl) - Complete CWL workflow
- [How-To: Genomics Workflows](../../../docs/how-to/genomics-workflows.md)
- [CWL Documentation](https://www.commonwl.org/)
