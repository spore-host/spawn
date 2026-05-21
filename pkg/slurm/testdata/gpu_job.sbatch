#!/bin/bash
#SBATCH --job-name=gpu-training
#SBATCH --time=04:00:00
#SBATCH --gres=gpu:1
#SBATCH --mem=32GB
#SBATCH --cpus-per-task=8
#SPAWN --spot=true
#SPAWN --region=us-east-1

# GPU training job
python train.py --epochs 100 --batch-size 64
