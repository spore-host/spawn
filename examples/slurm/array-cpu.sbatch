#!/bin/bash
#SBATCH --job-name=cpu-array
#SBATCH --array=1-100
#SBATCH --time=02:00:00
#SBATCH --mem=8G
#SBATCH --cpus-per-task=4
#SBATCH --output=logs/task-%a.out
#SBATCH --error=logs/task-%a.err

# CPU-intensive array job example
# Suitable for parameter sweeps, data processing, simulations

echo "Starting task ${SLURM_ARRAY_TASK_ID} on $(hostname)"
echo "CPUs: ${SLURM_CPUS_PER_TASK}"
echo "Memory: ${SLURM_MEM_PER_NODE}MB"

# Example: Run simulation with different parameters
PARAM_FILE="parameters.csv"
PARAMS=$(sed -n "${SLURM_ARRAY_TASK_ID}p" $PARAM_FILE)

echo "Parameters: $PARAMS"

# Run your computation
# ./my_simulation --params "$PARAMS" --output "results/task-${SLURM_ARRAY_TASK_ID}.dat"

# For testing:
sleep 10
echo "Task ${SLURM_ARRAY_TASK_ID} complete"
