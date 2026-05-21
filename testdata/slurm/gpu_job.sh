#!/bin/bash
#SBATCH --job-name=gpu-training
#SBATCH --gres=gpu:2
#SBATCH --time=02:00:00
#SBATCH --mem=32G
#SBATCH --cpus-per-task=8

module load cuda/11.8
python train_model.py --gpus 2
