version 1.0

## GATK Germline Variant Calling Pipeline
## Optimized for spawn with cost-efficient spot instances
##
## Pipeline: FASTQ → BAM → GVCF → VCF
## Runtime: ~4 hours for 30x WGS on c6i.4xlarge
## Cost: ~$0.50 per sample (spot)

workflow GatkGermline {
  input {
    String sample_name
    File fastq_r1
    File fastq_r2
    File reference_fasta
    File reference_fai
    File reference_dict
    File dbsnp_vcf
    File dbsnp_vcf_index
    File known_indels_vcf
    File known_indels_vcf_index
  }

  call FastQC as FastQC_R1 {
    input:
      fastq = fastq_r1,
      sample_name = sample_name + "_R1"
  }

  call FastQC as FastQC_R2 {
    input:
      fastq = fastq_r2,
      sample_name = sample_name + "_R2"
  }

  call BwaMemAlign {
    input:
      sample_name = sample_name,
      fastq_r1 = fastq_r1,
      fastq_r2 = fastq_r2,
      reference_fasta = reference_fasta,
      reference_fai = reference_fai,
      reference_dict = reference_dict
  }

  call MarkDuplicates {
    input:
      sample_name = sample_name,
      input_bam = BwaMemAlign.output_bam
  }

  call BaseRecalibrator {
    input:
      sample_name = sample_name,
      input_bam = MarkDuplicates.output_bam,
      input_bam_index = MarkDuplicates.output_bam_index,
      reference_fasta = reference_fasta,
      reference_fai = reference_fai,
      reference_dict = reference_dict,
      dbsnp_vcf = dbsnp_vcf,
      dbsnp_vcf_index = dbsnp_vcf_index,
      known_indels_vcf = known_indels_vcf,
      known_indels_vcf_index = known_indels_vcf_index
  }

  call ApplyBQSR {
    input:
      sample_name = sample_name,
      input_bam = MarkDuplicates.output_bam,
      input_bam_index = MarkDuplicates.output_bam_index,
      recal_table = BaseRecalibrator.recal_table,
      reference_fasta = reference_fasta,
      reference_fai = reference_fai,
      reference_dict = reference_dict
  }

  call HaplotypeCaller {
    input:
      sample_name = sample_name,
      input_bam = ApplyBQSR.output_bam,
      input_bam_index = ApplyBQSR.output_bam_index,
      reference_fasta = reference_fasta,
      reference_fai = reference_fai,
      reference_dict = reference_dict,
      dbsnp_vcf = dbsnp_vcf,
      dbsnp_vcf_index = dbsnp_vcf_index
  }

  call GenotypeGVCFs {
    input:
      sample_name = sample_name,
      input_gvcf = HaplotypeCaller.output_gvcf,
      input_gvcf_index = HaplotypeCaller.output_gvcf_index,
      reference_fasta = reference_fasta,
      reference_fai = reference_fai,
      reference_dict = reference_dict
  }

  call VariantFiltration {
    input:
      sample_name = sample_name,
      input_vcf = GenotypeGVCFs.output_vcf,
      input_vcf_index = GenotypeGVCFs.output_vcf_index,
      reference_fasta = reference_fasta,
      reference_fai = reference_fai,
      reference_dict = reference_dict
  }

  output {
    File fastqc_r1_html = FastQC_R1.html_report
    File fastqc_r2_html = FastQC_R2.html_report
    File aligned_bam = BwaMemAlign.output_bam
    File dedup_bam = MarkDuplicates.output_bam
    File dedup_metrics = MarkDuplicates.metrics
    File recal_bam = ApplyBQSR.output_bam
    File recal_table = BaseRecalibrator.recal_table
    File gvcf = HaplotypeCaller.output_gvcf
    File gvcf_index = HaplotypeCaller.output_gvcf_index
    File vcf = GenotypeGVCFs.output_vcf
    File vcf_index = GenotypeGVCFs.output_vcf_index
    File filtered_vcf = VariantFiltration.output_vcf
    File filtered_vcf_index = VariantFiltration.output_vcf_index
  }

  meta {
    author: "spawn genomics team"
    description: "GATK Best Practices germline variant calling pipeline"
  }
}

task FastQC {
  input {
    File fastq
    String sample_name
  }

  command <<<
    fastqc ~{fastq} -o . --threads 2
  >>>

  output {
    File html_report = glob("*_fastqc.html")[0]
    File zip_report = glob("*_fastqc.zip")[0]
  }

  runtime {
    docker: "quay.io/biocontainers/fastqc:0.12.1--hdfd78af_0"
    cpu: 2
    memory: "4 GB"
  }
}

task BwaMemAlign {
  input {
    String sample_name
    File fastq_r1
    File fastq_r2
    File reference_fasta
    File reference_fai
    File reference_dict
  }

  command <<<
    set -e

    # BWA MEM alignment
    bwa mem -t 16 -R "@RG\tID:~{sample_name}\tSM:~{sample_name}\tPL:ILLUMINA" \
      ~{reference_fasta} \
      ~{fastq_r1} \
      ~{fastq_r2} \
      | samtools view -b - \
      | samtools sort -@ 4 -o ~{sample_name}.bam -

    # Index
    samtools index ~{sample_name}.bam
  >>>

  output {
    File output_bam = "~{sample_name}.bam"
    File output_bam_index = "~{sample_name}.bam.bai"
  }

  runtime {
    docker: "quay.io/biocontainers/mulled-v2-fe8faa35dbf6dc65a0f7f5d4ea12e31a79f73e40:219b6c272b25e7e642ae3ff0bf0c5c81a5135ab4-0"
    cpu: 16
    memory: "32 GB"
  }
}

task MarkDuplicates {
  input {
    String sample_name
    File input_bam
  }

  command <<<
    gatk MarkDuplicates \
      --INPUT ~{input_bam} \
      --OUTPUT ~{sample_name}.dedup.bam \
      --METRICS_FILE ~{sample_name}.dedup.metrics.txt \
      --CREATE_INDEX true \
      --VALIDATION_STRINGENCY SILENT \
      --OPTICAL_DUPLICATE_PIXEL_DISTANCE 2500
  >>>

  output {
    File output_bam = "~{sample_name}.dedup.bam"
    File output_bam_index = "~{sample_name}.dedup.bai"
    File metrics = "~{sample_name}.dedup.metrics.txt"
  }

  runtime {
    docker: "broadinstitute/gatk:4.5.0.0"
    cpu: 4
    memory: "16 GB"
  }
}

task BaseRecalibrator {
  input {
    String sample_name
    File input_bam
    File input_bam_index
    File reference_fasta
    File reference_fai
    File reference_dict
    File dbsnp_vcf
    File dbsnp_vcf_index
    File known_indels_vcf
    File known_indels_vcf_index
  }

  command <<<
    gatk BaseRecalibrator \
      --input ~{input_bam} \
      --reference ~{reference_fasta} \
      --known-sites ~{dbsnp_vcf} \
      --known-sites ~{known_indels_vcf} \
      --output ~{sample_name}.recal.table
  >>>

  output {
    File recal_table = "~{sample_name}.recal.table"
  }

  runtime {
    docker: "broadinstitute/gatk:4.5.0.0"
    cpu: 4
    memory: "16 GB"
  }
}

task ApplyBQSR {
  input {
    String sample_name
    File input_bam
    File input_bam_index
    File recal_table
    File reference_fasta
    File reference_fai
    File reference_dict
  }

  command <<<
    gatk ApplyBQSR \
      --input ~{input_bam} \
      --reference ~{reference_fasta} \
      --bqsr-recal-file ~{recal_table} \
      --output ~{sample_name}.recal.bam \
      --create-output-bam-index true
  >>>

  output {
    File output_bam = "~{sample_name}.recal.bam"
    File output_bam_index = "~{sample_name}.recal.bai"
  }

  runtime {
    docker: "broadinstitute/gatk:4.5.0.0"
    cpu: 4
    memory: "16 GB"
  }
}

task HaplotypeCaller {
  input {
    String sample_name
    File input_bam
    File input_bam_index
    File reference_fasta
    File reference_fai
    File reference_dict
    File dbsnp_vcf
    File dbsnp_vcf_index
  }

  command <<<
    gatk HaplotypeCaller \
      --input ~{input_bam} \
      --reference ~{reference_fasta} \
      --dbsnp ~{dbsnp_vcf} \
      --emit-ref-confidence GVCF \
      --output ~{sample_name}.g.vcf.gz
  >>>

  output {
    File output_gvcf = "~{sample_name}.g.vcf.gz"
    File output_gvcf_index = "~{sample_name}.g.vcf.gz.tbi"
  }

  runtime {
    docker: "broadinstitute/gatk:4.5.0.0"
    cpu: 4
    memory: "16 GB"
  }
}

task GenotypeGVCFs {
  input {
    String sample_name
    File input_gvcf
    File input_gvcf_index
    File reference_fasta
    File reference_fai
    File reference_dict
  }

  command <<<
    gatk GenotypeGVCFs \
      --reference ~{reference_fasta} \
      --variant ~{input_gvcf} \
      --output ~{sample_name}.vcf.gz
  >>>

  output {
    File output_vcf = "~{sample_name}.vcf.gz"
    File output_vcf_index = "~{sample_name}.vcf.gz.tbi"
  }

  runtime {
    docker: "broadinstitute/gatk:4.5.0.0"
    cpu: 2
    memory: "8 GB"
  }
}

task VariantFiltration {
  input {
    String sample_name
    File input_vcf
    File input_vcf_index
    File reference_fasta
    File reference_fai
    File reference_dict
  }

  command <<<
    gatk VariantFiltration \
      --reference ~{reference_fasta} \
      --variant ~{input_vcf} \
      --filter-expression "QD < 2.0" --filter-name "QD2" \
      --filter-expression "FS > 60.0" --filter-name "FS60" \
      --filter-expression "MQ < 40.0" --filter-name "MQ40" \
      --filter-expression "MQRankSum < -12.5" --filter-name "MQRankSum-12.5" \
      --filter-expression "ReadPosRankSum < -8.0" --filter-name "ReadPosRankSum-8" \
      --output ~{sample_name}.filtered.vcf.gz
  >>>

  output {
    File output_vcf = "~{sample_name}.filtered.vcf.gz"
    File output_vcf_index = "~{sample_name}.filtered.vcf.gz.tbi"
  }

  runtime {
    docker: "broadinstitute/gatk:4.5.0.0"
    cpu: 2
    memory: "8 GB"
  }
}
