# MPI with Custom AMIs

This guide explains how to use MPI with custom AMIs, including creating MPI-ready AMIs and using them for fast cluster launches.

## Table of Contents
- [Why Use Custom AMIs with MPI](#why-use-custom-amis-with-mpi)
- [Creating an MPI-Ready AMI](#creating-an-mpi-ready-ami)
- [Launching MPI Clusters from Custom AMIs](#launching-mpi-clusters-from-custom-amis)
- [Custom MPI Implementations](#custom-mpi-implementations)
- [Performance Benefits](#performance-benefits)
- [Troubleshooting](#troubleshooting)

## Why Use Custom AMIs with MPI

**Benefits:**
1. **Faster cluster launch** - Pre-installed MPI saves 2-3 minutes per cluster
2. **Custom MPI builds** - Use specific versions, Intel MPI, or custom-compiled OpenMPI
3. **Pre-installed applications** - Include MPI applications with all dependencies
4. **Reproducibility** - Identical environment across multiple cluster launches
5. **Optimizations** - Enable CUDA, InfiniBand, or other hardware-specific features

## Creating an MPI-Ready AMI

### Basic Workflow

```bash
# Step 1: Launch a base instance
spawn launch --instance-type c7i.xlarge --name mpi-builder --region us-east-1

# Step 2: SSH and install software
spawn connect mpi-builder

# Install custom MPI (example: OpenMPI with CUDA support)
sudo yum install -y gcc gcc-c++ make wget
wget https://download.open-mpi.org/release/open-mpi/v4.1/openmpi-4.1.6.tar.gz
tar -xzf openmpi-4.1.6.tar.gz
cd openmpi-4.1.6
./configure --prefix=/opt/openmpi --with-cuda=/usr/local/cuda
make -j $(nproc)
sudo make install

# Add to PATH
echo 'export PATH=/opt/openmpi/bin:$PATH' | sudo tee /etc/profile.d/custom-mpi.sh
echo 'export LD_LIBRARY_PATH=/opt/openmpi/lib:$LD_LIBRARY_PATH' | sudo tee -a /etc/profile.d/custom-mpi.sh
source /etc/profile.d/custom-mpi.sh

# Verify installation
mpirun --version

# Optional: Install your MPI application
git clone https://github.com/myorg/mpi-simulation
cd mpi-simulation
make
sudo make install

exit

# Step 3: Create AMI
spawn create-ami mpi-builder \
  --name "openmpi-4.1.6-cuda-20260116" \
  --description "OpenMPI 4.1.6 with CUDA support" \
  --region us-east-1

# Get the AMI ID
spawn list-amis --name "openmpi-4.1.6"
```

### Example: Intel MPI

```bash
# On the builder instance
wget https://registrationcenter-download.intel.com/akdlm/IRC_NAS/...
sudo yum install -y ./intel-mpi-*.rpm
source /opt/intel/mpi/latest/env/vars.sh

# Make persistent
echo 'source /opt/intel/mpi/latest/env/vars.sh' | sudo tee /etc/profile.d/intel-mpi.sh
```

## Launching MPI Clusters from Custom AMIs

### With Auto-Detection (Recommended)

Spawn automatically detects if MPI is already installed:

```bash
spawn launch \
  --count 8 \
  --instance-type c7i.4xlarge \
  --job-array-name compute \
  --ami ami-0123456789abcdef0 \
  --mpi \
  --mpi-command "mpirun -np 128 ./my-mpi-app"
```

**What happens:**
1. Spawn checks for `mpirun` in PATH
2. If found: Skips OpenMPI installation
3. If not found: Installs OpenMPI from yum
4. Always configures SSH keys and hostfile
5. Runs your MPI command on leader node

### With Explicit Skip (Advanced)

Force skip installation even if MPI not detected:

```bash
spawn launch \
  --count 8 \
  --instance-type c7i.4xlarge \
  --job-array-name compute \
  --ami ami-0123456789abcdef0 \
  --mpi \
  --skip-mpi-install \
  --mpi-command "/opt/openmpi/bin/mpirun -np 128 ./my-mpi-app"
```

**Use cases:**
- MPI installed in non-standard location
- Custom MPI wrapper scripts
- Debugging MPI environment issues

## Custom MPI Implementations

### Intel MPI

```bash
# On builder instance
sudo yum install -y intel-mpi

# Create AMI
spawn create-ami builder --name "intel-mpi-20260116"

# Launch cluster (Intel MPI in PATH)
spawn launch \
  --count 8 \
  --ami ami-intel-mpi \
  --mpi \
  --mpi-command "mpirun -np 128 ./app"
```

### MPICH

```bash
# On builder instance
sudo yum install -y mpich mpich-devel
echo 'export PATH=/usr/lib64/mpich/bin:$PATH' | sudo tee /etc/profile.d/mpich.sh

# Create AMI
spawn create-ami builder --name "mpich-20260116"

# Launch cluster
spawn launch \
  --count 8 \
  --ami ami-mpich \
  --mpi \
  --mpi-command "mpirun -np 128 ./app"
```

### GPU-Aware MPI (CUDA)

```bash
# On GPU-enabled builder instance
wget https://download.open-mpi.org/release/open-mpi/v4.1/openmpi-4.1.6.tar.gz
tar -xzf openmpi-4.1.6.tar.gz
cd openmpi-4.1.6
./configure --prefix=/opt/openmpi --with-cuda=/usr/local/cuda --enable-mpi-cxx
make -j $(nproc)
sudo make install

# Create GPU-optimized AMI
spawn create-ami builder --name "openmpi-cuda-20260116"

# Launch GPU cluster
spawn launch \
  --count 4 \
  --instance-type p3.8xlarge \
  --ami ami-gpu-mpi \
  --mpi \
  --mpi-command "mpirun -np 16 --mca btl_openib_allow_ib 1 ./gpu-app"
```

## Performance Benefits

### Launch Time Comparison

**Base AMI (OpenMPI from yum):**
```
1. Instance launch:        ~60 seconds
2. User-data execution:    ~120 seconds (includes yum install)
3. MPI setup:              ~30 seconds
Total: ~210 seconds
```

**Custom AMI (MPI pre-installed):**
```
1. Instance launch:        ~60 seconds
2. User-data execution:    ~30 seconds (skips yum install)
3. MPI setup:              ~30 seconds
Total: ~120 seconds
```

**Savings:** 90 seconds per cluster launch (43% faster)

### Use Case: Iterative Development

If you launch 10 clusters per day for testing:
- Base AMI: 10 × 210s = 35 minutes waiting
- Custom AMI: 10 × 120s = 20 minutes waiting
- **Time saved: 15 minutes/day**

## Troubleshooting

### MPI Not Detected on Custom AMI

**Problem:** Spawn installs MPI even though it's in your AMI

**Solution:**
```bash
# Verify MPI in PATH
spawn connect instance-0
which mpirun
echo $PATH

# If mpirun not in PATH, add to /etc/profile.d/
echo 'export PATH=/opt/openmpi/bin:$PATH' | sudo tee /etc/profile.d/mpi.sh
```

### SSH Key Issues

**Problem:** MPI jobs can't ssh between nodes

**Status:** This should never happen - SSH keys are ALWAYS generated at boot time, not baked into AMI

**Debug:**
```bash
# On leader node
spawn connect job-0
cat /root/.ssh/id_rsa.pub
ssh job-1 hostname  # Should work without password
```

### Custom MPI Not in PATH

**Problem:** Using `--skip-mpi-install` but MPI command not found

**Solution:** Use full path in `--mpi-command`
```bash
spawn launch \
  --ami ami-custom \
  --mpi \
  --skip-mpi-install \
  --mpi-command "/opt/openmpi/bin/mpirun -np 64 ./app"
```

### Multiple MPI Versions

**Problem:** AMI has OpenMPI 4.0, user-data installs OpenMPI 5.0

**Solution:** Use `--skip-mpi-install` to prefer AMI version
```bash
spawn launch \
  --ami ami-openmpi-4.0 \
  --mpi \
  --skip-mpi-install
```

## Best Practices

### 1. Tag Your AMIs

Always tag AMIs with MPI information:

```bash
spawn create-ami builder \
  --name "openmpi-4.1.6-cuda" \
  --tag mpi:version=4.1.6 \
  --tag mpi:implementation=openmpi \
  --tag mpi:cuda=enabled
```

### 2. Test Before Creating AMI

Verify MPI works on the builder instance:

```bash
# Single-node test
mpirun -np 4 hostname

# Verify environment
mpirun --version
echo $PATH
```

### 3. Document Your AMIs

Include a README in `/etc/mpi-info`:

```bash
cat > /tmp/mpi-info <<EOF
MPI Implementation: OpenMPI 4.1.6
Installation Prefix: /opt/openmpi
Build Options: --with-cuda --enable-mpi-cxx
Applications: /opt/simulation/bin/simulate
Notes: Use with CUDA 11.8 or later
EOF
sudo mv /tmp/mpi-info /etc/mpi-info
```

### 4. Version Your AMIs

Use date-based naming:

```bash
openmpi-4.1.6-20260116  # Good
my-mpi-ami              # Bad (no version info)
```

### 5. Keep SSH Keys Out of AMIs

Never bake SSH keys into AMIs - Spawn generates them at boot time automatically.

## Example Workflows

### Workflow 1: Quick Testing with Custom MPI

```bash
# Create AMI with dependencies
spawn launch --name builder
spawn connect builder
# (install MPI + app)
exit
spawn create-ami builder --name "test-mpi-$(date +%Y%m%d)"

# Launch test cluster
AMI=$(spawn list-amis --name "test-mpi" --json | jq -r '.[0].ami_id')
spawn launch --count 4 --ami $AMI --mpi --job-array-name test

# Run test
spawn connect test-0
mpirun -np 16 -hostfile /tmp/mpi-hostfile ./test-program
```

### Workflow 2: Production MPI Application

```bash
# 1. Create production-ready AMI
spawn launch --instance-type c7i.2xlarge --name prod-builder
spawn connect prod-builder

# Install optimized MPI
./configure --prefix=/opt/openmpi --enable-mpi-cxx --with-cuda
make -j 16 && sudo make install

# Install application
git clone https://github.com/org/app && cd app && make && sudo make install
exit

# 2. Create versioned AMI
spawn create-ami prod-builder \
  --name "prod-app-v1.2.3-$(date +%Y%m%d)" \
  --description "Production app v1.2.3 with OpenMPI 4.1.6"

# 3. Launch production cluster
spawn launch \
  --count 32 \
  --instance-type c7i.8xlarge \
  --ami ami-prod \
  --mpi \
  --mpi-command "mpirun -np 1024 /opt/app/bin/production-run --input /efs/data" \
  --efs-id fs-production-data \
  --job-array-name prod-run

# 4. Monitor
spawn list --job-array-name prod-run
```

## Related Documentation

- [MPI Guide](../MPI_GUIDE.md) - General MPI usage
- [AMI Management](../README.md#ami-management) - Creating and managing AMIs
- [Job Arrays](../README.md#job-arrays) - Coordinated instance groups
- [Shared Storage](../SHARED_STORAGE.md) - Using FSx/EFS with MPI

## Support

For issues or questions:
- GitHub Issues: https://github.com/spore-host/spore-host/issues
- Tag issues with `component:spawn` and `type:bug` or `type:feature`
