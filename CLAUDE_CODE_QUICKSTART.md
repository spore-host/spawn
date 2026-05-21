# spawn - Claude Code Quick Start

## 🚀 Build and Test in Claude Code

### Step 1: Navigate to Project

```bash
cd /mnt/user-data/outputs/spawn
```

### Step 2: Build Everything

```bash
# Build for all platforms
make build-all

# You'll get:
# ✅ bin/spawn-linux-amd64
# ✅ bin/spawn-linux-arm64
# ✅ bin/spored-linux-amd64
# ✅ bin/spored-linux-arm64
# ✅ bin/spawn-darwin-amd64
# ✅ bin/spawn-darwin-arm64
# ✅ bin/spawn-windows-amd64.exe
```

### Step 3: Test Locally

```bash
# Test the wizard
./bin/spawn

# Or test with flags
./bin/spawn --help
```

### Step 4: Test with truffle (if available)

```bash
# Build truffle first
cd ../truffle
make build

# Test integration
./bin/truffle search t3.medium --pick-first --output json | \
  ../spawn/bin/spawn
```

### Step 5: Deploy spored to S3 (Optional)

```bash
cd /mnt/user-data/outputs/spawn

# Make deploy script executable
chmod +x scripts/deploy-spored.sh

# Deploy to all regions
./scripts/deploy-spored.sh 0.1.0

# This creates regional S3 buckets and uploads binaries
# Requires AWS credentials configured
```

---

## 🎯 What You Get

### Two Complete Tools

**1. truffle** - Find AWS instances
- Search by pattern
- Check Spot prices  
- Find ML capacity (Capacity Blocks & ODCRs)
- Multi-region support
- Python bindings (native cgo)

**2. spawn** - Launch instances effortlessly
- Interactive wizard (beginner-friendly)
- Pipe from truffle (power users)
- Direct flags (quick launches)
- Windows/Linux/macOS support
- spored agent for self-monitoring
- S3-based fast distribution

---

## 📋 Project Status

### ✅ Fully Implemented

**Core Features:**
- [x] spawn CLI with 3 input modes
- [x] spored systemd agent
- [x] Multi-architecture (x86_64 + ARM64)
- [x] AMI auto-detection (4 variants)
- [x] Hibernation support
- [x] TTL and idle monitoring
- [x] Parent-child resource tagging

**Enhancements:**
- [x] Interactive wizard
- [x] Windows 11 support
- [x] S3 regional distribution
- [x] Live progress display
- [x] Cost estimates
- [x] Auto SSH key creation

**Integration:**
- [x] Reads truffle JSON
- [x] Clean stdout for piping
- [x] Cross-platform paths

---

## 🧪 Testing Checklist

### Basic Functionality
```bash
# 1. Version check
./bin/spawn --version

# 2. Help text
./bin/spawn --help
./bin/spawn launch --help

# 3. Wizard (interactive)
./bin/spawn
# Press Ctrl+C to exit without launching

# 4. Flags (direct)
./bin/spawn --instance-type t3.medium --region us-east-1
# This will attempt to launch (needs AWS creds)
```

### Integration with truffle
```bash
# Test JSON piping
echo '{"instance_type":"t3.medium","region":"us-east-1"}' | ./bin/spawn

# With truffle
cd ../truffle
./bin/truffle search t3.medium --output json | ../spawn/bin/spawn
```

### Platform Detection
```bash
# Check platform detection
./bin/spawn --interactive
# Should detect correct OS and SSH paths
```

---

## 🎨 Demo Scenarios

### Scenario 1: Complete Beginner

```bash
./bin/spawn

# User sees:
# 🧙 spawn Setup Wizard
# Step-by-step questions
# Just press Enter for defaults
# Gets instance in 60 seconds!
```

### Scenario 2: Power User with truffle

```bash
# Find cheapest Spot instance
cd ../truffle
./bin/truffle spot "m7i.*" --sort-by-price --pick-first | \
  ../spawn/bin/spawn --ttl 8h

# Launches immediately, auto-terminates
```

### Scenario 3: GPU Training

```bash
# Find available GPU capacity
cd ../truffle
./bin/truffle capacity --instance-types p5.48xlarge --available-only | \
  ../spawn/bin/spawn --ttl 24h --hibernate-on-idle
```

---

## 🔧 Development Tips

### Quick Rebuild
```bash
make build           # Current platform only
make build-all       # All platforms
```

### Testing Wizard
```bash
# Run wizard without AWS creds
./bin/spawn

# Will work through all steps
# Only fails at actual launch (expected without creds)
```

### Testing Platform Detection
```bash
# On different platforms:
./bin/spawn --interactive

# Should show:
# Windows: C:\Users\...\.ssh\id_rsa
# Linux: ~/.ssh/id_rsa
# macOS: ~/.ssh/id_rsa
```

### Clean Build
```bash
make clean
make build-all
```

---

## 📦 File Locations

### Important Files
```
spawn/
├── cmd/launch.go           # Main launch logic (wizard + progress)
├── pkg/platform/           # Windows/Linux/macOS detection
├── pkg/wizard/             # Interactive wizard
├── pkg/progress/           # Live progress display
├── pkg/aws/ami.go          # AMI detection (4 variants)
├── scripts/deploy-spored.sh # S3 deployment
└── Makefile                # Build system
```

### Generated Files
```
bin/
├── spawn-linux-amd64
├── spawn-linux-arm64
├── spored-linux-amd64
├── spored-linux-arm64
├── spawn-darwin-amd64
├── spawn-darwin-arm64
└── spawn-windows-amd64.exe
```

---

## 🌟 Key Innovations

### 1. Three Input Modes
- **Wizard**: Beginner-friendly, guided
- **Pipe**: From truffle JSON
- **Flags**: Direct command-line

### 2. Cross-Platform Native
- Windows 11 (PowerShell)
- Linux (bash)
- macOS (zsh/bash)

### 3. S3 Distribution
- Regional buckets (fast)
- No rate limits
- Full control

### 4. Live Progress
- Real-time step updates
- Beautiful UI
- Cross-platform

### 5. Auto-Monitoring
- spored runs on instance
- Laptop-independent
- Self-terminates

---

## ✅ Pre-Flight Checklist

Before first use:

1. **AWS Credentials**
   ```bash
   export AWS_ACCESS_KEY_ID=...
   export AWS_SECRET_ACCESS_KEY=...
   export AWS_DEFAULT_REGION=us-east-1
   ```

2. **Build Binaries**
   ```bash
   make build-all
   ```

3. **Deploy spored** (optional but recommended)
   ```bash
   ./scripts/deploy-spored.sh 0.1.0
   ```

4. **Test Wizard**
   ```bash
   ./bin/spawn
   ```

---

## 🎉 Success Criteria

spawn is working if:

- ✅ `./bin/spawn --version` shows version
- ✅ `./bin/spawn` launches wizard
- ✅ Wizard shows 6 steps with defaults
- ✅ Platform detection works (shows correct paths)
- ✅ Can parse JSON from truffle
- ✅ Progress display shows live updates
- ✅ Builds for all platforms (including Windows)

---

## 📞 Next Steps

1. **Build**: `make build-all`
2. **Test wizard**: `./bin/spawn`
3. **Test with truffle**: See integration examples above
4. **Deploy to S3**: `./scripts/deploy-spored.sh 0.1.0`
5. **Ship it**: Give users Windows/Linux/macOS binaries!

**Ready to make AWS accessible to everyone!** 🚀
