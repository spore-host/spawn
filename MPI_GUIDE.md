# MPI (Message Passing Interface) Guide

## Quick Start

Launch a 4-node MPI cluster and run a simple command:

```bash
spawn launch \
  --count 4 \
  --instance-type t3.medium \
  --job-array-name mpi-test \
  --mpi \
  --mpi-command "hostname" \
  --ttl 30m \
  --region us-east-1
```

This will:
1. Launch 4 EC2 instances as a job array
2. Install OpenMPI on all nodes
3. Configure passwordless SSH between nodes
4. Generate an MPI hostfile
5. Run `mpirun -np 8 -hostfile /tmp/mpi-hostfile hostname` on the leader node
6. Auto-terminate after 30 minutes

## What is MPI?

MPI (Message Passing Interface) is a standard for parallel computing across multiple machines. It allows you to:

- Run the same program on multiple nodes simultaneously
- Communicate between processes on different machines
- Distribute large computations across a cluster

Common use cases:
- Scientific simulations (weather, molecular dynamics, CFD)
- Machine learning training on large datasets
- Data processing pipelines
- Monte Carlo simulations
- Parameter sweeps

## How spawn's MPI Support Works

### Job Arrays Foundation

MPI support builds on spawn's job array feature:
- All nodes launch simultaneously
- Peer discovery via `/etc/spawn/job-array-peers.json`
- Environment variables: `$JOB_ARRAY_INDEX`, `$JOB_ARRAY_SIZE`

### MPI-Specific Setup

When `--mpi` flag is used, spawn:

1. **Creates MPI Security Group**: Auto-creates `spawn-mpi-{name}` security group that allows:
   - All TCP traffic (ports 0-65535) from instances in the same security group
   - SSH (port 22) from anywhere for user access

2. **Installs OpenMPI**: Uses Amazon Linux 2023 package (OpenMPI 4.1.2)
   ```bash
   yum install -y openmpi openmpi-devel
   ```

3. **Configures SSH Keys**:
   - Leader (index 0) generates SSH key pair
   - Uploads public key to S3: `s3://spawn-binaries-{region}/mpi-keys/{job-array-id}/id_rsa.pub`
   - Workers download and add to `~/.ssh/authorized_keys`
   - S3 lifecycle policy auto-deletes keys after 1 day

4. **Generates Hostfile**:
   - Waits for peer discovery to complete
   - Creates `/tmp/mpi-hostfile` with format:
     ```
     172.31.1.10 slots=2
     172.31.1.11 slots=2
     172.31.1.12 slots=2
     172.31.1.13 slots=2
     ```
   - Slots = number of vCPUs (override with `--mpi-processes-per-node`)

5. **Leader Runs Command**:
   - Node 0 executes: `mpirun -np {total_slots} -hostfile /tmp/mpi-hostfile {command}`
   - Workers wait for MPI to SSH to them and launch processes

## Usage Examples

### Basic MPI Cluster (No Command)

Launch cluster for interactive use:

```bash
spawn launch \
  --count 8 \
  --instance-type c7i.4xlarge \
  --job-array-name compute \
  --mpi \
  --ttl 4h

# SSH to leader
ssh ec2-user@<leader-ip>

# Run MPI programs manually
mpirun -np 128 -hostfile /tmp/mpi-hostfile ./my-simulation
```

### Run MPI Command at Launch

Execute command automatically when cluster is ready:

```bash
spawn launch \
  --count 16 \
  --instance-type c7i.2xlarge \
  --job-array-name molecular-dynamics \
  --mpi \
  --mpi-command "/opt/namd/namd2 simulation.conf" \
  --ttl 8h
```

### Custom Processes Per Node

Override vCPU count for hyperthreading or memory-bound workloads:

```bash
# Use only half the vCPUs (reduce memory pressure)
spawn launch \
  --count 4 \
  --instance-type c7i.8xlarge \
  --mpi \
  --mpi-processes-per-node 16 \
  --job-array-name memory-intensive

# Or oversubscribe for I/O-bound workloads
spawn launch \
  --count 4 \
  --instance-type c7i.4xlarge \
  --mpi \
  --mpi-processes-per-node 32 \
  --job-array-name io-bound
```

### With Spot Instances

Save ~70% on compute costs:

```bash
spawn launch \
  --count 32 \
  --instance-type c7i.xlarge \
  --spot \
  --mpi \
  --mpi-command "mpirun hostname" \
  --ttl 2h
```

### Large-Scale Cluster

Launch 100+ node cluster:

```bash
spawn launch \
  --count 128 \
  --instance-type c7i.large \
  --job-array-name large-scale \
  --mpi \
  --ttl 1h
```

## MPI Environment

### Available on All Nodes

- OpenMPI 4.1.2
- MPI compiler wrappers: `mpicc`, `mpic++`, `mpifort`
- Environment variables set in `/etc/profile.d/mpi.sh`:
  ```bash
  export PATH=/usr/lib64/openmpi/bin:$PATH
  export LD_LIBRARY_PATH=/usr/lib64/openmpi/lib:$LD_LIBRARY_PATH
  export OMPI_MCA_plm_rsh_agent=ssh
  export OMPI_ALLOW_RUN_AS_ROOT=1
  export OMPI_ALLOW_RUN_AS_ROOT_CONFIRM=1
  ```

### SSH Configuration

Passwordless SSH is configured for root user:
- Leader's private key: `/root/.ssh/id_rsa`
- StrictHostKeyChecking disabled for MPI use
- SSH between nodes uses private IPs (faster, no egress charges)

### Hostfile Location

MPI hostfile is at `/tmp/mpi-hostfile` on all nodes.

## Writing MPI Programs

### Hello World Example

```c
// hello_mpi.c
#include <mpi.h>
#include <stdio.h>

int main(int argc, char** argv) {
    MPI_Init(&argc, &argv);

    int rank, size;
    MPI_Comm_rank(MPI_COMM_WORLD, &rank);
    MPI_Comm_size(MPI_COMM_WORLD, &size);

    char hostname[256];
    gethostname(hostname, 256);

    printf("Hello from rank %d/%d on %s\n", rank, size, hostname);

    MPI_Finalize();
    return 0;
}
```

Compile and run:
```bash
# On leader node
mpicc hello_mpi.c -o hello_mpi
mpirun -np 16 -hostfile /tmp/mpi-hostfile ./hello_mpi
```

### Point-to-Point Communication

```c
// ping_pong.c
#include <mpi.h>
#include <stdio.h>

int main(int argc, char** argv) {
    MPI_Init(&argc, &argv);

    int rank;
    MPI_Comm_rank(MPI_COMM_WORLD, &rank);

    int count = 10;

    if (rank == 0) {
        MPI_Send(&count, 1, MPI_INT, 1, 0, MPI_COMM_WORLD);
        MPI_Recv(&count, 1, MPI_INT, 1, 0, MPI_COMM_WORLD, MPI_STATUS_IGNORE);
        printf("Rank 0 received: %d\n", count);
    } else if (rank == 1) {
        MPI_Recv(&count, 1, MPI_INT, 0, 0, MPI_COMM_WORLD, MPI_STATUS_IGNORE);
        count *= 2;
        MPI_Send(&count, 1, MPI_INT, 0, 0, MPI_COMM_WORLD);
    }

    MPI_Finalize();
    return 0;
}
```

### Collective Operations

```c
// reduce_sum.c
#include <mpi.h>
#include <stdio.h>

int main(int argc, char** argv) {
    MPI_Init(&argc, &argv);

    int rank;
    MPI_Comm_rank(MPI_COMM_WORLD, &rank);

    int local_value = rank + 1;
    int sum;

    MPI_Reduce(&local_value, &sum, 1, MPI_INT, MPI_SUM, 0, MPI_COMM_WORLD);

    if (rank == 0) {
        printf("Sum of all ranks: %d\n", sum);
    }

    MPI_Finalize();
    return 0;
}
```

## Troubleshooting

### SSH Connection Failures

**Symptom**: `mpirun` fails with "Permission denied" or "Connection refused"

**Solutions**:
1. Check security group: `spawn-mpi-{name}` must allow TCP from itself
2. Verify SSH keys distributed:
   ```bash
   # On leader
   cat /root/.ssh/id_rsa.pub

   # On worker
   cat /root/.ssh/authorized_keys
   ```
3. Test manual SSH:
   ```bash
   # From leader to worker
   ssh 172.31.1.11 hostname
   ```

### MPI Routing Errors

**Symptom**: "ORTE does not know how to route a message"

**Solution**: This was fixed in spawn v0.6.0. Ensure you're using the latest version.

### Hostfile Not Found

**Symptom**: `/tmp/mpi-hostfile: No such file or directory`

**Solution**: Wait for peer discovery to complete. Check `/etc/spawn/job-array-peers.json` exists:
```bash
cat /etc/spawn/job-array-peers.json
```

### Wrong Number of Processes

**Symptom**: MPI runs with unexpected number of processes

**Check**:
```bash
# Verify hostfile
cat /tmp/mpi-hostfile

# Check vCPU count
nproc

# Override with flag
spawn launch --mpi --mpi-processes-per-node 8 ...
```

### Out of Memory

**Symptom**: MPI processes killed, OOM errors in logs

**Solutions**:
1. Reduce processes per node: `--mpi-processes-per-node 2`
2. Use instance type with more RAM
3. Increase swap space in user-data

## Performance Tips

### Choose Right Instance Type

- **CPU-bound**: c7i.xlarge and above (compute optimized)
- **Memory-bound**: r7i.xlarge and above (memory optimized)
- **Network-bound**: c7gn.16xlarge (100 Gbps network)

### Process-to-Core Mapping

Default: 1 MPI process per vCPU. Override for:
- **Memory-intensive**: Use fewer processes (`--mpi-processes-per-node 1`)
- **I/O-intensive**: Use more processes (oversubscribe)
- **Hybrid MPI+OpenMP**: Use fewer MPI ranks, more threads per rank

### Network Considerations

- Private IPs used automatically (fast, free)
- All instances in same AZ for lowest latency
- Security group allows full bandwidth between nodes

## Future Enhancements

### Placement Groups (Planned)

```bash
# Low latency networking
spawn launch --mpi --placement-group ...
```

Benefits:
- Sub-microsecond latency
- 10 Gbps bandwidth per flow
- Same AZ required

### EFA Networking (Planned)

```bash
# High-performance fabric
spawn launch --mpi --efa --instance-type c5n.18xlarge ...
```

Benefits:
- 100 Gbps bandwidth
- OS-bypass networking
- < 10 μs latency

### Shared Storage (Planned)

```bash
# Mount EFS on all nodes
spawn launch --mpi --efs-id fs-12345678 ...
```

Benefits:
- Share binaries and input data
- Centralized output collection
- NFS-compatible

## Cost Analysis

Example: 8-node cluster, 4 hours

**On-Demand (c7i.4xlarge)**:
- 8 × $0.68/hr × 4hr = $21.76

**Spot (70% savings)**:
- 8 × $0.20/hr × 4hr = $6.40

**Network**: ~$0 (private IPs, no egress)
**Storage**: ~$0.01 (SSH key in S3)

Total: ~$6.40 with Spot instances

## Additional Resources

- OpenMPI Documentation: https://www.open-mpi.org/doc/
- MPI Tutorial: https://mpitutorial.com/
- spawn Job Arrays: `JOB_ARRAYS.md`
- spawn Issue #28: MPI support implementation

## Feedback

Found a bug or have a feature request? Please create an issue:
https://github.com/spore-host/spore-host/issues
