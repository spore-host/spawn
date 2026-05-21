#!/bin/bash
#SBATCH --job-name=array-test
#SBATCH --array=1-100
#SBATCH --time=02:00:00
#SBATCH --mem=16GB
#SBATCH --cpus-per-task=4
#SBATCH --output=results/job_%A_%a.out

# Array job example
python analyze.py --task $SLURM_ARRAY_TASK_ID --input data/input_${SLURM_ARRAY_TASK_ID}.txt
