#!/bin/bash
#SBATCH --job-name=genome-analysis
#SBATCH --array=1-50
#SBATCH --time=04:00:00
#SBATCH --mem=32G
#SBATCH --cpus-per-task=8
#SBATCH --partition=bioinformatics

# Bioinformatics pipeline example
# Analyzes 50 samples against reference genome

echo "Analyzing sample ${SLURM_ARRAY_TASK_ID}"

# Reference genome (100GB) - perfect for data staging!
REFERENCE="/mnt/data/hg38-reference.fasta"
SAMPLE="sample-${SLURM_ARRAY_TASK_ID}.fastq"
OUTPUT="aligned-${SLURM_ARRAY_TASK_ID}.sam"

# Load tools (cluster)
# module load bwa/0.7.17
# module load samtools/1.15

# On spawn with data staging:
# spawn stage upload hg38-reference.fasta --regions us-east-1,us-west-2
# spawn slurm submit bioinformatics.sbatch --spot

# Align reads
bwa mem -t ${SLURM_CPUS_PER_TASK} $REFERENCE $SAMPLE > $OUTPUT

# Convert to BAM
samtools view -@ ${SLURM_CPUS_PER_TASK} -bS $OUTPUT > aligned-${SLURM_ARRAY_TASK_ID}.bam

# Sort
samtools sort -@ ${SLURM_CPUS_PER_TASK} aligned-${SLURM_ARRAY_TASK_ID}.bam \
  -o sorted-${SLURM_ARRAY_TASK_ID}.bam

echo "Sample ${SLURM_ARRAY_TASK_ID} complete"
