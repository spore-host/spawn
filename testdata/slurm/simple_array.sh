#!/bin/bash
#SBATCH --job-name=test-array
#SBATCH --array=1-10
#SBATCH --time=01:00:00
#SBATCH --mem=4G
#SBATCH --cpus-per-task=2

echo "Running task ${SLURM_ARRAY_TASK_ID}"
./compute_task.sh $SLURM_ARRAY_TASK_ID
