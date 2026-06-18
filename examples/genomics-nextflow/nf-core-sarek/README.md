# nf-core/sarek on spawn (via nf-spawn)

Run the popular [nf-core/sarek](https://nf-co.re/sarek) germline and somatic
variant-calling pipeline on spawn using the
[**nf-spawn**](https://github.com/spore-host/nf-spawn) executor plugin — each
pipeline process runs on its own right-sized, auto-terminating EC2 instance, for
a large cost saving vs. AWS HealthOmics.

**Pipeline:** nf-core/sarek v3.4 (FASTQ → VCF)
**Supported:** germline, somatic, CNV, SV calling

> **How this differs from the old approach.** nf-spawn is now spawn's
> first-class Nextflow integration: you run `nextflow` locally and the plugin
> turns each process into an ephemeral instance. You no longer build a
> "Nextflow AMI" or run the whole pipeline inside one big spawned box.

---

## Pipeline overview

nf-core/sarek includes:
- **Preprocessing:** trim, QC, align (BWA-MEM)
- **Variant calling:** GATK HaplotypeCaller, Mutect2, Strelka, Manta
- **Copy number:** ASCAT, Control-FREEC
- **Structural variants:** Manta, TIDDIT
- **Annotation:** VEP, SnpEff

---

## Prerequisites

- [`spawn`](https://github.com/spore-host/spawn) installed and on `PATH`
- AWS credentials configured
- Nextflow 26.04.x (the version nf-spawn tracks) on your laptop or head node
- An S3 bucket for the work directory

No custom AMI is required — nf-spawn can install Docker and host packages on a
stock AL2023 image per task (see `ext.ensureDocker` / `ext.packages` in the
[nf-spawn README](https://github.com/spore-host/nf-spawn#per-process-ext-options)).

---

## Quick start

**1. `nextflow.config` — enable nf-spawn and size the sarek process labels:**

```groovy
plugins {
    id 'nf-spawn@0.8.0'
}

process {
    executor = 'spawn'

    ext.region = 'us-east-1'
    ext.ttl    = '12h'        // safety backstop; instances self-terminate on completion
    ext.spot   = true

    // Map sarek's resource labels onto instance types
    withLabel: process_low    { ext.instanceType = 'c7g.large'   }
    withLabel: process_medium { ext.instanceType = 'c7g.2xlarge' }
    withLabel: process_high   { ext.instanceType = 'c7g.8xlarge' }
}

// Shared work dir across all task instances
workDir = 's3://my-bucket/nextflow-work'
```

**2. Sample sheet (`samplesheet.csv`):**

```csv
patient,sample,lane,fastq_1,fastq_2
NA12878,NA12878,L001,s3://data/NA12878_L001_R1.fastq.gz,s3://data/NA12878_L001_R2.fastq.gz
```

**3. Run it — locally; nf-spawn launches an instance per process:**

```bash
nextflow run nf-core/sarek -r 3.4.0 \
  -profile docker \
  -c nextflow.config \
  --input samplesheet.csv \
  --genome GRCh38 \
  --tools haplotypecaller,strelka,manta \
  --outdir s3://my-results/sarek/ \
  -resume
```

`-resume` re-uses completed tasks, so an interrupted run (or a spot reclaim of a
single task instance) picks up where it left off.

---

## Delivering reference data

sarek needs reference bundles (GRCh38, known-sites VCFs). With nf-spawn, attach
them as a read-only EBS snapshot or a shared FSx/EFS filesystem rather than
re-downloading per task — see
[Delivering reference data](https://github.com/spore-host/nf-spawn#delivering-reference-data)
in the nf-spawn README for the `ext.volumes` / `ext.fsx` / `ext.efs` patterns and
the wide-fan-out tradeoffs.

---

## Migrating from AWS HealthOmics

HealthOmics Nextflow workflows run on spawn with minimal change:

1. **Remove** HealthOmics-specific profiles.
2. **Add** the nf-spawn config above (plugin + `executor = 'spawn'` + per-label
   instance types).
3. **Update** S3 input/output paths.

No pipeline (`.nf`) code changes are required — only `nextflow.config`.

---

## See also

- [nf-spawn](https://github.com/spore-host/nf-spawn) — the executor plugin
- [Generic Nextflow example](../../workflows/nextflow/) — minimal nf-spawn pipeline
- [nf-core/sarek docs](https://nf-co.re/sarek)
- [Nextflow docs](https://www.nextflow.io/docs/latest/)
