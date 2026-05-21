# How-To: Debugging Failed Instances

Troubleshoot common instance failures and errors.

## Find Failed Instances

### List Failed Instances

```bash
# All failed instances
spawn list --state terminated --exit-code 1

# Failed instances in specific sweep
spawn status sweep-20260127-abc123 --filter state=failed
```

**Output:**
```
INSTANCE-ID          NAME       STATE       EXIT-CODE  RUNTIME
i-0abc123def456789   run-023    terminated  1          45m
i-0abc234def567890   run-045    terminated  1          12m
i-0abc345def678901   run-078    terminated  137        2h 15m
```

### Get Failure Details

```bash
spawn status i-0abc123def456789 --json | jq '.failure_reason'
```

**Output:**
```json
{
  "exit_code": 1,
  "error_message": "CUDA out of memory",
  "last_log_lines": [
    "[15:32:10] Loading model...",
    "[15:32:15] Allocating 12GB tensor...",
    "[15:32:16] RuntimeError: CUDA out of memory"
  ]
}
```

---

## Check Logs

### Cloud-Init Logs

User data script output:

```bash
# Connect to instance (if still running)
spawn connect i-0abc123def456789

# View cloud-init output
tail -100 /var/log/cloud-init-output.log

# View full log
less /var/log/cloud-init-output.log

# Search for errors
grep -i error /var/log/cloud-init-output.log
```

**Common patterns:**
```
Cloud-init v. 23.1.2 running 'modules:final' at Tue, 27 Jan 2026 15:00:00 +0000
[15:00:05] Starting user script...
[15:00:10] Downloading data from S3...
[15:00:45] Processing data...
[15:32:16] ERROR: CUDA out of memory
Cloud-init v. 23.1.2 finished at Tue, 27 Jan 2026 15:32:16 +0000. Result: error
```

### Spored Logs

spawn agent logs:

```bash
# View spored logs
journalctl -u spored -n 100

# Follow in real-time
journalctl -u spored -f

# Show errors only
journalctl -u spored -p err
```

### System Logs

```bash
# System messages
tail -100 /var/log/messages

# Kernel messages (OOM killer, etc.)
dmesg | tail -50

# SSH logs
tail -50 /var/log/secure
```

---

## Common Errors

### 1. Out of Memory (OOM)

**Symptoms:**
```
Killed
bash: line 1: 12345 Killed     python train.py
```

**Check for OOM:**
```bash
dmesg | grep -i "out of memory"
dmesg | grep -i "oom"
```

**Output:**
```
[12345.678] Out of memory: Killed process 12345 (python) total-vm:32GB
```

**Solution:**
```bash
# Use larger instance type
spawn launch --instance-type m7i.xlarge  # 16 GB instead of 8 GB

# Or reduce batch size
python train.py --batch-size 16  # instead of 64
```

---

### 2. CUDA Out of Memory

**Symptoms:**
```
RuntimeError: CUDA out of memory. Tried to allocate 12.00 GiB
```

**Check GPU memory:**
```bash
nvidia-smi
```

**Output:**
```
+-----------------------------------------------------------------------------+
| NVIDIA-SMI 525.85.12    Driver Version: 525.85.12    CUDA Version: 12.0     |
|-------------------------------+----------------------+----------------------+
| GPU  Name        Persistence-M| Bus-Id        Disp.A | Volatile Uncorr. ECC |
| Fan  Temp  Perf  Pwr:Usage/Cap|         Memory-Usage | GPU-Util  Compute M. |
|===============================+======================+======================|
|   0  NVIDIA A10G         On   | 00000000:00:1E.0 Off |                    0 |
|  0%   32C    P0    54W / 300W |  23040MiB / 23040MiB |      0%      Default |
+-------------------------------+----------------------+----------------------+
GPU memory: 23040 MB used / 23040 MB total (100%)
```

**Solutions:**

**1. Use larger GPU:**
```bash
spawn launch --instance-type g5.2xlarge  # 48 GB instead of 24 GB
```

**2. Reduce batch size:**
```python
# train.py
parser.add_argument('--batch-size', type=int, default=16)  # Reduce from 64
```

**3. Use gradient accumulation:**
```python
# Simulate larger batch size without memory increase
for i, batch in enumerate(dataloader):
    loss = model(batch)
    loss = loss / accumulation_steps
    loss.backward()

    if (i + 1) % accumulation_steps == 0:
        optimizer.step()
        optimizer.zero_grad()
```

**4. Use mixed precision:**
```python
from torch.cuda.amp import autocast, GradScaler

scaler = GradScaler()

for batch in dataloader:
    with autocast():
        loss = model(batch)

    scaler.scale(loss).backward()
    scaler.step(optimizer)
    scaler.update()
```

---

### 3. File Not Found

**Symptoms:**
```
FileNotFoundError: [Errno 2] No such file or directory: '/data/input.csv'
```

**Check file exists:**
```bash
ls -la /data/input.csv
ls -la /data/
```

**Common causes:**

**A. S3 download failed:**
```bash
# Check S3 download in user data
aws s3 ls s3://my-bucket/data/input.csv

# Download with error checking
aws s3 cp s3://my-bucket/data/input.csv /data/input.csv || {
  echo "ERROR: S3 download failed"
  exit 1
}
```

**B. Wrong path:**
```bash
# Script expects /data/input.csv
# But downloaded to /tmp/input.csv

# Fix:
aws s3 cp s3://bucket/input.csv /data/input.csv  # Correct path
```

**C. File not created yet:**
```bash
# Script A creates file
# Script B reads file
# But Script A hasn't finished

# Fix: Add wait or check
while [ ! -f /data/input.csv ]; do
  echo "Waiting for input file..."
  sleep 5
done
```

---

### 4. Permission Denied

**Symptoms:**
```
PermissionError: [Errno 13] Permission denied: '/results/output.txt'
```

**Check permissions:**
```bash
ls -la /results/
ls -la /results/output.txt
```

**Solutions:**

**A. Directory doesn't exist:**
```bash
mkdir -p /results
chmod 777 /results
```

**B. Wrong user:**
```bash
# Running as ec2-user but directory owned by root
sudo chown -R ec2-user:ec2-user /results

# Or run command as correct user
su - ec2-user -c "python train.py"
```

**C. Read-only filesystem:**
```bash
# Check mounts
mount | grep /results

# Remount as read-write
sudo mount -o remount,rw /results
```

---

### 5. Module Not Found

**Symptoms:**
```
ModuleNotFoundError: No module named 'torch'
```

**Check Python environment:**
```bash
python3 --version
pip3 list | grep torch
which python3
```

**Solutions:**

**A. Install missing package:**
```bash
pip3 install torch torchvision
```

**B. Wrong Python environment:**
```bash
# User data uses python3
# But virtual env not activated

# Fix: Activate venv
source /opt/venv/bin/activate
python train.py
```

**C. Add to user data:**
```yaml
user_data: |
  #!/bin/bash
  set -e

  # Install dependencies
  pip3 install torch torchvision numpy pandas

  # Then run script
  python3 train.py
```

---

### 6. Timeout

**Symptoms:**
```
Job exceeded timeout of 4h
Process killed by spored: timeout
```

**Check runtime:**
```bash
spawn status i-0abc123def456789 --json | jq '.runtime'
# Output: "4h 0m 5s"
```

**Solutions:**

**A. Increase timeout:**
```bash
spawn launch --ttl 8h  # Increase from 4h
```

**B. Optimize code:**
```python
# Profile to find slow parts
import cProfile
cProfile.run('train_model()', 'profile.stats')

# Or use profiler
from torch.profiler import profile
with profile() as prof:
    train_model()
print(prof.key_averages())
```

**C. Use faster instance:**
```bash
spawn launch --instance-type c7i.xlarge  # CPU-optimized
```

---

### 7. SSH Connection Failed

**Symptoms:**
```
ssh: connect to host 54.123.45.67 port 22: Connection refused
```

**Check instance status:**
```bash
spawn status i-0abc123def456789
```

**Common causes:**

**A. Instance still initializing:**
```
State: running
Status Checks: 1/2 passed (initializing)
```

**Solution: Wait**
```bash
spawn connect i-0abc123def456789 --wait
```

**B. Security group blocks SSH:**
```bash
# Check security group
aws ec2 describe-security-groups --group-ids sg-xxx

# Add SSH rule
aws ec2 authorize-security-group-ingress \
  --group-id sg-xxx \
  --protocol tcp \
  --port 22 \
  --cidr $(curl -s ifconfig.me)/32
```

**C. Wrong SSH key:**
```bash
# Use correct key
spawn connect i-0abc123def456789 --key ~/.ssh/my-key
```

---

### 8. Disk Full

**Symptoms:**
```
OSError: [Errno 28] No space left on device
```

**Check disk usage:**
```bash
df -h
```

**Output:**
```
Filesystem      Size  Used Avail Use% Mounted on
/dev/xvda1       8G    8G     0 100% /
```

**Solutions:**

**A. Increase volume size:**
```bash
spawn launch --volume-size 100  # 100 GB instead of 8 GB
```

**B. Clean up temp files:**
```bash
# Remove cache
rm -rf ~/.cache/pip
rm -rf /tmp/*

# Remove old logs
journalctl --vacuum-time=1d
```

**C. Store data on S3:**
```bash
# Don't download everything to disk
# Stream from S3 instead
aws s3 cp s3://bucket/large-file.csv - | python process.py
```

---

### 9. Network Timeout

**Symptoms:**
```
ConnectionError: HTTPSConnectionPool(host='s3.amazonaws.com', port=443): Max retries exceeded
```

**Check connectivity:**
```bash
# Test S3 connectivity
aws s3 ls s3://my-bucket/

# Test internet
curl -I https://google.com

# Check DNS
nslookup s3.amazonaws.com
```

**Solutions:**

**A. Retry with backoff:**
```python
import time
from botocore.exceptions import ClientError

def download_with_retry(bucket, key, max_retries=5):
    for attempt in range(max_retries):
        try:
            s3.download_file(bucket, key, '/tmp/file')
            return
        except ClientError as e:
            if attempt < max_retries - 1:
                wait_time = 2 ** attempt
                print(f"Retry {attempt + 1}/{max_retries} after {wait_time}s")
                time.sleep(wait_time)
            else:
                raise
```

**B. Use VPC endpoint for S3:**
```bash
# Create VPC endpoint (in console or CLI)
# Improves S3 connectivity and reduces costs
```

---

### 10. Exit Code 137 (OOM Killer)

**Symptoms:**
```
Exit code: 137
Process killed by kernel
```

**Explanation:**
Exit code 137 = 128 + 9 (SIGKILL from OOM killer)

**Confirm OOM kill:**
```bash
dmesg | grep -i "out of memory"
```

**Solutions:**
Same as "Out of Memory" above.

---

## Debug Checklist

When instance fails:

1. **Check exit code**
   ```bash
   spawn status i-xxx --json | jq '.exit_code'
   ```

2. **Read logs**
   ```bash
   spawn connect i-xxx
   tail -100 /var/log/cloud-init-output.log
   ```

3. **Check system resources**
   ```bash
   # Memory
   free -h
   dmesg | grep -i oom

   # Disk
   df -h

   # GPU (if applicable)
   nvidia-smi
   ```

4. **Verify inputs**
   ```bash
   # Check files exist
   ls -la /data/

   # Check S3 access
   aws s3 ls s3://my-bucket/
   ```

5. **Test locally**
   ```bash
   # SSH in and run commands manually
   python3 train.py --debug
   ```

---

## Prevention

### Add Error Handling

```bash
#!/bin/bash
set -e  # Exit on any error
set -o pipefail  # Exit on pipe failures

# Function for errors
handle_error() {
  echo "ERROR on line $1"
  aws s3 cp /var/log/cloud-init-output.log \
    s3://my-bucket/errors/$(hostname)-$(date +%s).log
  exit 1
}

trap 'handle_error $LINENO' ERR

# Your script
python train.py
```

### Validate Before Running

```bash
#!/bin/bash
set -e

# Check prerequisites
if ! command -v python3 &> /dev/null; then
  echo "ERROR: python3 not found"
  exit 1
fi

if ! aws s3 ls s3://my-bucket/data/ &> /dev/null; then
  echo "ERROR: Cannot access S3 bucket"
  exit 1
fi

if [ ! -d /data ]; then
  echo "ERROR: /data directory missing"
  exit 1
fi

# Proceed with confidence
python3 train.py
```

### Use Dry Run Mode

```bash
# Test without actually launching
spawn launch --param-file sweep.yaml --dry-run
```

---

## Getting Help

If stuck, gather this information:

1. **Instance ID**
2. **Error message** (exact text)
3. **Exit code**
4. **Relevant log snippets**
5. **Instance type and configuration**
6. **What you tried**

Post to:
- [GitHub Issues](https://github.com/spore-host/spore-host/issues)
- Include all information above

---

## See Also

- [Tutorial 7: Monitoring & Alerts](../tutorials/07-monitoring-alerts.md) - Monitor instances
- [spawn status](../reference/commands/status.md) - Status command reference
- [FAQ](../FAQ.md) - Common issues and solutions
