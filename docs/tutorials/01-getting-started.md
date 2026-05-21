# Tutorial 1: Getting Started with spawn

**Duration:** 15 minutes
**Level:** Beginner
**Prerequisites:** AWS account, basic command line knowledge

## What You'll Learn

In this tutorial, you'll:
- Install spawn on your machine
- Configure AWS credentials
- Launch your first EC2 instance
- Connect to it via SSH
- Terminate the instance to avoid charges

By the end, you'll understand the basic spawn workflow and be ready to launch instances on your own.

## Before You Start

**What You Need:**
- An AWS account ([create one here](https://aws.amazon.com/free/))
- Command line access (Terminal on macOS/Linux, PowerShell on Windows)
- 15 minutes of focused time

**Cost Note:** This tutorial uses a `t3.micro` instance which costs ~$0.01/hour. We'll run it for about 10 minutes, costing less than $0.01.

## Step 1: Install spawn

### macOS/Linux

```bash
# Download the latest release
curl -LO https://github.com/spore-host/spore-host/releases/latest/download/spawn-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m)

# Make it executable
chmod +x spawn-*

# Move to PATH
sudo mv spawn-* /usr/local/bin/spawn

# Verify installation
spawn --version
```

**Expected output:**
```
spawn version 0.13.1
```

### Windows

```powershell
# Download from GitHub releases
# https://github.com/spore-host/spore-host/releases/latest/download/spawn-windows-amd64.exe

# Rename to spawn.exe and add to PATH
# Or run directly: .\spawn-windows-amd64.exe
```

### Build from Source (Optional)

```bash
git clone https://github.com/spore-host/spore-host
cd spore-host/spawn
make build
sudo make install
```

## Step 2: Configure AWS Credentials

spawn uses your AWS credentials to launch instances. Let's set them up.

### Check Existing Credentials

```bash
# Check if you have AWS CLI configured
aws sts get-caller-identity
```

If this works, you're all set! Skip to Step 3.

### First-Time AWS Setup

If you don't have AWS CLI configured:

1. **Create an IAM User** (if you don't have one):
   - Go to [AWS IAM Console](https://console.aws.amazon.com/iam/)
   - Users → Add users
   - Name: `spawn-user`
   - Access type: Programmatic access
   - Permissions: Attach `PowerUserAccess` policy (or see [IAM_PERMISSIONS.md](../../IAM_PERMISSIONS.md) for minimal policy)

2. **Save Credentials**:
   - Copy Access Key ID and Secret Access Key

3. **Configure AWS CLI**:

```bash
# Install AWS CLI if needed
# macOS: brew install awscli
# Linux: sudo apt-get install awscli
# Windows: Download from https://aws.amazon.com/cli/

# Configure credentials
aws configure
```

**Enter when prompted:**
```
AWS Access Key ID: <your-access-key>
AWS Secret Access Key: <your-secret-key>
Default region name: us-east-1
Default output format: json
```

4. **Verify Setup**:

```bash
aws sts get-caller-identity
```

**Expected output:**
```json
{
    "UserId": "AIDAXXXXXXXXXXXXXXXXX",
    "Account": "123456789012",
    "Arn": "arn:aws:iam::123456789012:user/spawn-user"
}
```

✅ Credentials configured!

## Step 3: Launch Your First Instance

Now for the exciting part - launching an instance!

### Basic Launch

```bash
spawn launch --instance-type t3.micro --ttl 1h
```

**What this does:**
- Launches a `t3.micro` instance (cheapest option, great for learning)
- Sets TTL (time-to-live) to 1 hour (auto-terminates after 1 hour)
- Auto-detects the right AMI (Amazon Linux 2023)
- Auto-creates SSH keys if you don't have them
- Auto-creates networking (VPC, subnet, security group)

**Expected output:**
```
🚀 Launching EC2 instance...

Configuration:
  Instance Type: t3.micro
  Region: us-east-1
  AMI: ami-0abc123def456789 (Amazon Linux 2023)
  TTL: 1h

Progress:
  ✓ Creating SSH key pair (default-ssh-key)
  ✓ Creating security group (spawn-sg-...)
  ✓ Launching instance
  ✓ Waiting for instance to start...
  ✓ Installing spored agent

Instance launched successfully! 🎉

Instance ID: i-0123456789abcdef0
Public IP: 54.123.45.67
DNS: i-0123456789abcdef0.c0zxr0ao.spore.host

Cost: $0.0104/hour (~$0.01/hour)
Estimated 1h cost: ~$0.01

Connect:
  spawn connect i-0123456789abcdef0

The instance will auto-terminate in 1 hour.
```

✅ Instance launched!

**What Happened Behind the Scenes:**
1. spawn chose the latest Amazon Linux 2023 AMI
2. Created an SSH key pair (stored in `~/.ssh/`)
3. Created a security group allowing SSH access
4. Launched the instance
5. Tagged it for automatic cleanup
6. Installed spored agent for self-monitoring

## Step 4: Connect to Your Instance

Let's connect via SSH:

```bash
spawn connect i-0123456789abcdef0
```

**Replace `i-0123456789abcdef0` with your actual instance ID from the previous step.**

**Expected output:**
```
Connecting to i-0123456789abcdef0...
  ✓ Instance is running
  ✓ SSH is ready

   __                 _       ___     ___ ____  ____  ____
  (_   _ _  _  _    |_ _ _  |_ (_   |_  (_  ) |_  ) |_  )
  __) |_)(_|| |L|   |_ (_|  |__(_)  |__ (_  ) (_  ) (_  )

  Amazon Linux 2023

[ec2-user@ip-10-0-1-100 ~]$
```

🎉 You're now connected to your EC2 instance!

### Try Some Commands

```bash
# Check system info
uname -a

# Check CPU info
lscpu | grep "Model name"

# Check memory
free -h

# Check spored agent status
systemctl status spored

# Exit when done
exit
```

**Example session:**
```bash
[ec2-user@ip-10-0-1-100 ~]$ uname -a
Linux ip-10-0-1-100 6.1.0-1234.amzn2023.x86_64

[ec2-user@ip-10-0-1-100 ~]$ lscpu | grep "Model name"
Model name: Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz

[ec2-user@ip-10-0-1-100 ~]$ free -h
              total        used        free      shared  buff/cache   available
Mem:          945Mi       234Mi       456Mi       1.0Mi       345Mi       678Mi
Swap:            0B          0B          0B

[ec2-user@ip-10-0-1-100 ~]$ exit
logout
Connection to 54.123.45.67 closed.
```

## Step 5: Check Instance Status

Back on your local machine, check the instance status:

```bash
spawn status i-0123456789abcdef0
```

**Expected output:**
```
Instance: i-0123456789abcdef0
Name: (none)
Region: us-east-1
State: running

Instance Type: t3.micro
Public IP: 54.123.45.67
Private IP: 10.0.1.100

Lifecycle:
  Launch Time: 2026-01-27 10:00:00 PST
  Uptime: 5m 23s
  TTL: 1h (54m 37s remaining)
  Auto-terminate at: 2026-01-27 11:00:00 PST

Cost:
  Hourly: $0.0104
  Current: $0.0009 (5.3 minutes)
  Projected 1h: $0.0104
```

## Step 6: Extend TTL (Optional)

If you need more time before the instance terminates:

```bash
# Add 30 minutes to TTL
spawn extend i-0123456789abcdef0 30m
```

**Expected output:**
```
Extending TTL for instance i-0123456789abcdef0...
  ✓ Current TTL: 54m
  ✓ Extension: 30m
  ✓ New TTL: 1h 24m

Instance will now run for approximately 1h 24m from now.
```

## Step 7: Terminate the Instance

When you're done experimenting, terminate the instance to stop charges:

```bash
# List all your instances
spawn list

# Terminate specific instance
aws ec2 terminate-instances --instance-ids i-0123456789abcdef0
```

**Expected output:**
```
{
    "TerminatingInstances": [
        {
            "InstanceId": "i-0123456789abcdef0",
            "CurrentState": {
                "Code": 32,
                "Name": "shutting-down"
            },
            "PreviousState": {
                "Code": 16,
                "Name": "running"
            }
        }
    ]
}
```

**Or wait for auto-termination:**

Since we set `--ttl 1h`, the instance will automatically terminate after 1 hour. No action needed!

## What You Learned

Congratulations! You've completed your first spawn tutorial. You learned:

✅ How to install spawn
✅ How to configure AWS credentials
✅ How to launch an EC2 instance with spawn
✅ How to connect via SSH
✅ How to check instance status
✅ How to extend TTL
✅ How to terminate instances

## Key Concepts

**TTL (Time-to-Live):**
- Instances auto-terminate after TTL expires
- Prevents forgotten instances from accruing charges
- Can be extended with `spawn extend`

**spored Agent:**
- Runs on each instance as a systemd service
- Monitors TTL, idle timeout, spot interruptions
- Self-terminates when conditions met
- No laptop connection required

**Auto-Cleanup:**
- spawn tags all resources (VPC, security groups, SSH keys)
- Everything automatically cleaned up on termination
- No orphaned resources left behind

## Common Issues

### "Insufficient permissions"

**Problem:** AWS user lacks required permissions.

**Solution:** Attach `PowerUserAccess` policy or see [IAM_PERMISSIONS.md](../../IAM_PERMISSIONS.md) for minimal policy.

### "SSH connection failed"

**Problem:** Instance may still be initializing.

**Solution:** Wait 30 seconds and try again. Use `--wait` flag:
```bash
spawn connect i-xxx --wait
```

### "No SSH key found"

**Problem:** spawn couldn't find or create SSH keys.

**Solution:** Create manually:
```bash
ssh-keygen -t rsa -b 2048 -f ~/.ssh/id_rsa -N ""
```

## Next Steps

Now that you've launched your first instance, continue learning:

📖 **[Tutorial 2: Your First Instance](02-first-instance.md)** - Learn about instance types, AMIs, security groups, and SSH keys

🛠️ **[How-To: Launch Instances](../how-to/launch-instances.md)** - Practical recipes for common launch scenarios

📚 **[Command Reference: launch](../reference/commands/launch.md)** - Complete flag documentation

## Cost Summary

**What did this tutorial cost?**
- Instance: t3.micro for ~10 minutes = ~$0.002
- EBS volume: 8 GB for ~10 minutes = ~$0.0001
- **Total: < $0.01** ✅

## Quick Reference

```bash
# Launch instance
spawn launch --instance-type t3.micro --ttl 1h

# Connect
spawn connect <instance-id>

# Check status
spawn status <instance-id>

# Extend TTL
spawn extend <instance-id> 30m

# List instances
spawn list

# Terminate
aws ec2 terminate-instances --instance-ids <instance-id>
```

---

**Feedback?** Found an issue or have suggestions? [Open an issue](https://github.com/spore-host/spore-host/issues/new?labels=type:docs,component:spawn)

**Next:** [Tutorial 2: Your First Instance](02-first-instance.md) →
