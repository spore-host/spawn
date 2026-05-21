# spawn Enhancement Guide: S3 + Windows + Wizard

Three major features that make spawn accessible to everyone!

## 🪣 Feature 1: S3-Based spored Distribution

### Why S3 Over GitHub Releases?

**Advantages:**
- ✅ **Regional buckets** → Fast in-region downloads (10-50ms vs 200-500ms)
- ✅ **No rate limits** → GitHub has 60 requests/hour
- ✅ **Full control** → You own the distribution
- ✅ **Cost-effective** → ~$0.01/month for binaries
- ✅ **Reliable** → AWS SLA
- ✅ **Versioning** → Built-in S3 versioning

### Architecture

```
Instance boots in us-east-1
  ↓
User-data detects region and architecture
  ↓
Downloads: s3://spawn-binaries-us-east-1/spored-linux-amd64
  ↓ (in-region, ~20ms)
Installs and starts spored
  ↓
Ready in <1 minute
```

### S3 Bucket Structure

```
spawn-binaries-us-east-1/       # Regional bucket
├── spored-linux-amd64          # Latest (main)
├── spored-linux-arm64          # Latest (main)
└── versions/
    ├── 0.1.0/
    │   ├── spored-linux-amd64
    │   └── spored-linux-arm64
    └── 0.2.0/
        ├── spored-linux-amd64
        └── spored-linux-arm64

spawn-binaries-us-west-2/      # Replicated
├── spored-linux-amd64
└── ...

spawn-binaries-eu-west-1/      # Replicated
├── spored-linux-amd64
└── ...
```

### Deployment Workflow

```bash
# 1. Build all architectures
make build-all

# 2. Deploy to all regions
./scripts/deploy-spored.sh 0.2.0

# Output:
# ✅ Deployed to us-east-1
# ✅ Deployed to us-west-2
# ✅ Deployed to eu-west-1
# ... (10 regions)

# 3. Instances automatically download from their region
# No configuration needed!
```

### User-Data Implementation

```bash
# Auto-detects region and architecture
REGION=$(curl http://169.254.169.254/latest/meta-data/placement/region)
ARCH=$(uname -m)

# Downloads from regional bucket
aws s3 cp s3://spawn-binaries-${REGION}/spored-linux-${ARCH} \
  /usr/local/bin/spored --region $REGION

# Fallback to us-east-1 if regional bucket doesn't exist
```

### Cost Analysis

**Storage:**
- 2 binaries × 10MB each × 10 regions = 200 MB
- Cost: $0.023/GB/month × 0.2 GB = **$0.005/month**

**Data Transfer:**
- 100 launches/day × 10MB = 1 GB/day
- In-region transfer: **FREE**
- Cross-region fallback: $0.02/GB = $0.60/month (rare)

**Total: ~$0.01/month** 🎉

### Setup Instructions

```bash
# 1. Create buckets (one-time)
./scripts/deploy-spored.sh 0.1.0

# This creates:
# - spawn-binaries-us-east-1
# - spawn-binaries-us-west-2
# - spawn-binaries-eu-west-1
# - ... (all regions)

# 2. Enable public read (one-time per region)
aws s3api put-bucket-policy \
  --bucket spawn-binaries-us-east-1 \
  --policy '{
    "Statement": [{
      "Effect": "Allow",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::spawn-binaries-us-east-1/*"
    }]
  }'

# 3. Future updates - just deploy
make build-all
./scripts/deploy-spored.sh 0.2.0
```

---

## 🪟 Feature 2: Windows 11 Support

### Why Windows Matters

**User base:**
- Data scientists (many use Windows)
- Corporate developers
- Students
- Game developers
- ~70% of desktop users

### Platform Detection

```go
// Detects OS automatically
platform.Detect()
  → Windows: C:\Users\username\.ssh\id_rsa
  → Linux:   ~/.ssh/id_rsa
  → macOS:   ~/.ssh/id_rsa
```

### Windows-Specific Handling

#### SSH Key Paths

```go
// Windows
SSHDir:        "C:\\Users\\username\\.ssh"
SSHKeyPath:    "C:\\Users\\username\\.ssh\\id_rsa"
SSHPubKeyPath: "C:\\Users\\username\\.ssh\\id_rsa.pub"

// Uses OpenSSH for Windows (Windows 10+)
SSHClient: "ssh.exe"
```

#### SSH Key Creation

```go
// Uses ssh-keygen.exe (comes with Windows)
exec.Command("ssh-keygen.exe",
    "-t", "rsa",
    "-b", "4096",
    "-f", "C:\\Users\\username\\.ssh\\id_rsa",
    "-N", "")
```

#### Terminal Colors

```go
// Enable ANSI escape sequences on Windows
func EnableWindowsColors() {
    // Works on Windows 10+ with modern terminals
    // Windows Terminal, PowerShell 7, etc.
}
```

#### Config/Log Paths

```go
// Windows
Config: %APPDATA%\spawn\config.toml
        (C:\Users\username\AppData\Roaming\spawn\config.toml)

Logs:   %LOCALAPPDATA%\spawn\logs
        (C:\Users\username\AppData\Local\spawn\logs)

// Linux/macOS
Config: ~/.spawn/config.toml
Logs:   ~/.spawn/logs
```

### Cross-Platform Commands

```go
// Generates correct SSH command for each platform
platform.GetSSHCommand("ec2-user", "54.123.45.67")

// Windows:
"ssh.exe -i C:/Users/username/.ssh/id_rsa ec2-user@54.123.45.67"

// Linux/macOS:
"ssh -i ~/.ssh/id_rsa ec2-user@54.123.45.67"
```

### Windows User Experience

```powershell
# PowerShell on Windows 11
PS C:\> spawn

╔════════════════════════════════════════════════════════╗
║  🧙 spawn Setup Wizard                                ║
╚════════════════════════════════════════════════════════╝

I'll help you launch an AWS EC2 instance!
Press Enter to use the default shown in [brackets]

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
📦 Step 1 of 6: Choose Instance Type
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Common choices:
  💻 Development & Testing:
     • t3.medium     - $0.04/hr  (2 vCPU, 4 GB)
...

# Works identically on Windows, Linux, and macOS!
```

### Building for Windows

```bash
# Build Windows executable
GOOS=windows GOARCH=amd64 go build -o spawn.exe main.go

# Result: spawn.exe (works on Windows 10+)
```

---

## 🧙 Feature 3: Interactive Wizard Mode

### Why Wizard Mode?

**Problem:** AWS is intimidating for non-experts
- Too many choices
- Complex terminology  
- Fear of mistakes
- Fear of surprise bills

**Solution:** Guided wizard that asks simple questions

### Wizard Flow

```
spawn (no arguments)
  ↓
Auto-detects: No input, terminal connected
  ↓
Launches wizard
  ↓
Step 1: Instance type (with recommendations)
Step 2: Region (with explanations)
Step 3: Spot vs On-Demand (with pros/cons)
Step 4: Auto-termination (with examples)
Step 5: SSH key (auto-create if missing)
Step 6: Name (optional)
  ↓
Shows summary with cost estimate
  ↓
Confirms
  ↓
Launches with live progress
  ↓
Shows SSH command
```

### Wizard Features

#### 1. **Smart Defaults**

```
Instance type [t3.medium]:              ← Just press Enter
Region [us-east-1]:                     ← Just press Enter
Use Spot? [y/N]:                        ← Just press Enter
Choice [3]:                             ← Just press Enter (both TTL + idle)
Time limit [8h]:                        ← Just press Enter
Idle timeout [1h]:                      ← Just press Enter
```

**Result:** User can press Enter 6 times → instance launches!

#### 2. **Educational**

```
💰 Step 3 of 6: Spot or On-Demand?

💡 Spot instances are up to 70% cheaper but can be interrupted.

   ✅ Good for: Development, testing, fault-tolerant workloads
   ⚠️  Not for: Production databases, critical services
```

#### 3. **Cost-Aware**

```
📋 Configuration Summary

You're about to launch:
  Instance Type:  t3.medium
  Region:         us-east-1
  Type:           Spot (up to 70% cheaper)
  Time Limit:     8h
  Idle Timeout:   1h

💰 Estimated cost: ~$0.01/hour (65% savings vs On-Demand)
   Total for 8h: ~$0.08

🚀 Launch instance? [Y/n]:
```

#### 4. **SSH Key Management**

```
🔑 Step 5 of 6: SSH Key Setup

✅ Found existing SSH key: C:\Users\alice\.ssh\id_rsa
   Will use this key for connecting to your instance

# Or if not found:

⚠️  No SSH key found at: C:\Users\alice\.ssh\id_rsa

   An SSH key is required to connect to your instance.

   Create one now? [Y/n]: y

  🔧 Creating SSH key...
  ✅ SSH key created at: C:\Users\alice\.ssh\id_rsa
```

### Live Progress Display

```
╔════════════════════════════════════════════════════════╗
║  🚀 Spawning Instance...                               ║
╚════════════════════════════════════════════════════════╝

  ✅ Detecting AMI (0.5s)
  ✅ Setting up SSH key (0.3s)
  ⏭️  Creating security group
  ✅ Launching instance (2.1s)
  ⏳ Installing spored agent (30.0s)
  ⏸️  Waiting for instance
  ⏸️  Getting public IP
  ⏸️  Waiting for SSH
```

**Updates in real-time** as steps complete!

### Final Success Screen

```
╔════════════════════════════════════════════════════════╗
║  🎉 Instance Ready!                                    ║
╚════════════════════════════════════════════════════════╝

Instance Details:

  Instance ID:  i-1234567890abcdef0
  Public IP:    54.123.45.67
  Status:       running

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🔌 Connect Now:

  ssh -i ~/.ssh/id_rsa ec2-user@54.123.45.67

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

💡 Automatic Monitoring:

   ⏰ Will terminate after: 8h
   💤 Will terminate if idle: 1h

   The spored agent is monitoring your instance.
   You can close your laptop - it will handle everything!

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

### Three Modes of Operation

#### Mode 1: Wizard (Interactive)

```bash
spawn
# Or explicitly:
spawn --interactive

# Guides through all steps
# Perfect for beginners
```

#### Mode 2: Pipe (from truffle)

```bash
truffle search m7i.large | spawn

# Skips wizard
# Uses truffle's JSON
# Shows live progress
```

#### Mode 3: Flags (Direct)

```bash
spawn --instance-type m7i.large --region us-east-1 --ttl 8h

# Skips wizard
# Uses flags
# Shows live progress
```

---

## 🎯 Combined User Experience

### Example: Complete First-Time Flow

```bash
# Windows 11 user, never used AWS
PS C:\> spawn

# Wizard detects Windows, starts
# Guides through setup
# Creates SSH key automatically
# Shows cost estimates
# Confirms

# Live progress shows each step
# spored downloads from S3 (fast!)
# SSH command ready

# User connects:
PS C:\> ssh -i C:\Users\alice\.ssh\id_rsa ec2-user@54.123.45.67

# Instance auto-terminates after 8h
# No surprise bills!
```

### Example: Power User

```bash
# Linux power user, wants GPU
$ truffle capacity --instance-types p5.48xlarge --available-only | \
  spawn --ttl 24h --hibernate-on-idle

# No wizard, direct launch
# spored from S3 (regional bucket)
# Ready in 60 seconds
```

---

## 📊 Feature Comparison

| Feature | Before | After |
|---------|--------|-------|
| **Distribution** | GitHub (slow, rate limits) | S3 (fast, regional) |
| **Platform** | Linux/macOS only | + Windows 11 |
| **UX** | Flags only | Wizard + Flags + Pipe |
| **SSH Setup** | Manual | Auto-detect/create |
| **Cost Visibility** | None | Estimates shown |
| **Progress** | Silent | Live updates |
| **First-time UX** | Confusing | Guided |

---

## 🚀 Implementation Status

### ✅ Completed

1. **Platform Detection** (`pkg/platform/platform.go`)
   - Windows/Linux/macOS detection
   - SSH key path handling
   - Config/log path handling

2. **Wizard** (`pkg/wizard/wizard.go`)
   - 6-step guided setup
   - Cost estimates
   - Educational content
   - Smart defaults

3. **Progress Display** (`pkg/progress/progress.go`)
   - Live step updates
   - Time tracking
   - Success screen
   - Cross-platform (Windows compatible)

4. **S3 Deployment** (`scripts/deploy-spored.sh`)
   - Regional bucket creation
   - Multi-region deployment
   - Versioning support

5. **Updated User-Data** (in `cmd/launch.go`)
   - Regional S3 downloads
   - Architecture detection
   - Fallback to us-east-1

### 🎉 Result

**spawn is now:**
- ✅ Windows 11 compatible
- ✅ Beginner-friendly (wizard)
- ✅ Power-user friendly (flags/pipe)
- ✅ Fast (S3 regional)
- ✅ Educational (cost estimates)
- ✅ Safe (auto-termination)
- ✅ Cross-platform (Go ftw!)

**Perfect for EVERYONE who needs compute!** 🌟
