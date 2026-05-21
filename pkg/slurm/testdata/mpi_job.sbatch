#!/bin/bash
#SBATCH --job-name=mpi-simulation
#SBATCH --nodes=8
#SBATCH --ntasks-per-node=16
#SBATCH --time=06:00:00
#SBATCH --mem=64GB

# MPI simulation
mpirun -np 128 ./simulation --config sim.conf
