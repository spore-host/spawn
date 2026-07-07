# Variant Calling Pipeline (CWL)

> ✅ **CWL is a first-class workflow integration on spawn**, via
> [**cwl-spawn**](https://github.com/spore-host/cwl-spawn) — the CWL analog of
> [nf-spawn](https://github.com/spore-host/nf-spawn) (Nextflow) and
> [miniwdl-spawn](https://github.com/spore-host/miniwdl-spawn) (WDL). Each CWL
> `CommandLineTool` step runs on a purpose-sized, ephemeral EC2 instance that
> auto-terminates, with inputs/outputs bridged through S3. The end-to-end path is
> verified (see cwl-spawn's `examples/hello.cwl`); this variant-calling pipeline
> is the next migration target (its own follow-up — the biological workflow files
> here are illustrative).

Germline variant calling pipeline using Common Workflow Language (CWL) on spawn.

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

### Install

```bash
pip install cwl-spawn
```

`cwl-spawn` drives `cwltool` as a library; the `spawn` and `truffle` CLIs must be
on your `PATH` and AWS credentials configured. Each `CommandLineTool` step's EC2
instance is auto-sized from its CWL `ResourceRequirement` (`coresMin`/`ramMin`)
via `truffle`, or pinned with a `spore.host` `InstanceType` hint.

### Run

```bash
export SPAWN_WORKDIR_S3=s3://my-bucket/cwl-runs   # the S3 bridge (required)
export SPAWN_REGION=us-east-1
export SPAWN_TTL=4h                                # TTL backstop per step

cwl-spawn --outdir results/ variant-calling.cwl inputs.yml
```

**`inputs.yml`:**

```yaml
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

sample_name: "NA12878"
```

`cwltool` handles parsing, scheduling, and output collection as usual; cwl-spawn
only swaps the local job runner so each step executes on its own ephemeral
instance and self-terminates when done (`--on-complete terminate` + TTL).

> **Auto-terminate caveat:** a very short step can finish before spawn's
> completion tag is readable, in which case the instance falls back to the TTL for
> teardown (spore-host/spawn#270). Always set `SPAWN_TTL` as the cost backstop.

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

- [**cwl-spawn**](https://github.com/spore-host/cwl-spawn) — the CWL execution backend used here
- [Nextflow on spawn (nf-spawn)](../../genomics-nextflow/nf-core-sarek/) — the Nextflow path
- [WDL on spawn (miniwdl-spawn)](https://github.com/spore-host/miniwdl-spawn) — the WDL path
- [CWL Documentation](https://www.commonwl.org/)
