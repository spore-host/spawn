#!/bin/bash
#SBATCH --job-name=gpu-training
#SBATCH --gres=gpu:v100:2
#SBATCH --time=08:00:00
#SBATCH --mem=64G
#SBATCH --cpus-per-task=16
#SBATCH --output=training-%j.out

# GPU training job example
# Requests 2× V100 GPUs with 64GB RAM

echo "Starting GPU training on $(hostname)"
echo "GPUs: ${CUDA_VISIBLE_DEVICES}"

# Load modules (on cluster)
# module load cuda/11.8
# module load python/3.11

# On spawn, CUDA is pre-installed in AL2023 GPU AMI
# Verify GPU availability
nvidia-smi

# Run training
python train_model.py \
  --gpus 2 \
  --batch-size 128 \
  --epochs 100 \
  --data /mnt/data/training-set/ \
  --output /mnt/results/model.pt

echo "Training complete"
