# Tutorial 2: Your First Instance

**Duration:** 20 minutes
**Level:** Beginner
**Prerequisites:** [Tutorial 1: Getting Started](01-getting-started.md)

## What You'll Learn

In this tutorial, you'll dive deeper into instance launching:
- Choose the right instance type for your workload
- Understand and select AMIs (Amazon Machine Images)
- Configure security groups and SSH access
- Manage instance lifecycle (stop, start, hibernate)
- Use instance tags for organization

## Instance Types: Choosing the Right One

AWS offers hundreds of instance types optimized for different workloads. Let's understand the naming scheme and make smart choices.

### Instance Type Naming

Format: `<family><generation>.<size>`

**Example: `m7i.large`**
- `m` = Family (general purpose)
- `7` = Generation (7th generation)
- `i` = Processor (Intel)
- `large` = Size (2 vCPU, 8 GB RAM)

### Common Families

| Family | Purpose | Use Cases | Cost |
|--------|---------|-----------|------|
| **t3/t4g** | Burstable | Web servers, dev/test, small databases | $ |
| **m7i/m7a** | General purpose | Balanced compute/memory, most workloads | $$ |
| **c7i/c7a** | Compute optimized | HPC, batch processing, gaming | $$ |
| **r7i/r7a** | Memory optimized | Databases, caching, analytics | $$$ |
| **g5/g6** | GPU instances | ML training, graphics rendering | $$$$ |
| **p5/p4** | High-end GPU | Large-scale ML, scientific computing | $$$$$ |

### Choosing for Your Workload

**Development/Testing:**
```bash
# Cheapest option, good for learning
spawn launch --instance-type t3.micro --ttl 2h

# More power for compilation
spawn launch --instance-type t3.large --ttl 4h
```

**Web Application:**
```bash
# Production web server
spawn launch --instance-type m7i.large --ttl 24h
```

**Data Processing:**
```bash
# CPU-intensive batch job
spawn launch --instance-type c7i.xlarge --ttl 8h
```

**ML Training:**
```bash
# GPU for deep learning
spawn launch --instance-type g5.xlarge --ttl 12h
```

### Exercise: Launch Different Sizes

Let's launch a few instances to see the differences:

```bash
# 1. Micro (1 vCPU, 1 GB RAM)
spawn launch --instance-type t3.micro --name micro-test --ttl 30m

# 2. Large (2 vCPU, 8 GB RAM)
spawn launch --instance-type t3.large --name large-test --ttl 30m

# List them
spawn list
```

Connect to each and compare:
```bash
# Connect to micro
spawn connect micro-test
cat /proc/cpuinfo | grep processor | wc -l  # Shows 1 vCPU
free -h  # Shows ~1 GB RAM
exit

# Connect to large
spawn connect large-test
cat /proc/cpuinfo | grep processor | wc -l  # Shows 2 vCPU
free -h  # Shows ~8 GB RAM
exit
```

## AMIs: Understanding Machine Images

An AMI (Amazon Machine Image) is a template containing:
- Operating system
- Pre-installed software
- Configuration settings

### Default AMI Selection

spawn auto-selects the appropriate AMI:

```bash
# x86_64 instance → Amazon Linux 2023 x86_64
spawn launch --instance-type m7i.large

# ARM instance → Amazon Linux 2023 ARM64
spawn launch --instance-type m8g.xlarge

# GPU instance → Amazon Linux 2023 GPU (with NVIDIA drivers)
spawn launch --instance-type g5.xlarge
```

### Using Specific AMIs

```bash
# Ubuntu 22.04
spawn launch --instance-type t3.micro --ami ami-0c7217cdde317cfec

# Amazon Linux 2
spawn launch --instance-type t3.micro --ami ami-0aa7d40eeae50c9a9
```

### Finding AMI IDs

**AWS Console:**
1. Go to EC2 → AMIs
2. Search for "Amazon Linux 2023"
3. Copy AMI ID

**AWS CLI:**
```bash
# Latest Amazon Linux 2023
aws ssm get-parameter \
  --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
  --query 'Parameter.Value' \
  --output text
```

## Security Groups: Controlling Access

Security groups act as virtual firewalls controlling inbound/outbound traffic.

### Default Security Group

spawn auto-creates a security group allowing:
- **Inbound:** SSH (port 22) from your IP
- **Outbound:** All traffic

### Custom Security Groups

**Create security group manually:**
```bash
# Create group
SG_ID=$(aws ec2 create-security-group \
  --group-name my-app-sg \
  --description "My application security group" \
  --vpc-id vpc-xxx \
  --query 'GroupId' \
  --output text)

# Allow SSH from your IP
MY_IP=$(curl -s ifconfig.me)
aws ec2 authorize-security-group-ingress \
  --group-id "$SG_ID" \
  --protocol tcp \
  --port 22 \
  --cidr "$MY_IP/32"

# Allow HTTP from anywhere
aws ec2 authorize-security-group-ingress \
  --group-id "$SG_ID" \
  --protocol tcp \
  --port 80 \
  --cidr 0.0.0.0/0

# Use with spawn
spawn launch --instance-type t3.micro --security-groups "$SG_ID"
```

**Common Ports:**
- 22: SSH
- 80: HTTP
- 443: HTTPS
- 3000: Node.js dev server
- 8080: Alternative HTTP
- 5432: PostgreSQL
- 3306: MySQL

### Exercise: Web Server with HTTP Access

```bash
# Launch instance
spawn launch --instance-type t3.micro --name web-server --ttl 1h

# Get instance ID
INSTANCE_ID=$(spawn list --format json | jq -r '.[0].instance_id')

# Get security group
SG_ID=$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" \
  --query 'Reservations[0].Instances[0].SecurityGroups[0].GroupId' \
  --output text)

# Add HTTP access
aws ec2 authorize-security-group-ingress \
  --group-id "$SG_ID" \
  --protocol tcp \
  --port 80 \
  --cidr 0.0.0.0/0

# Connect and start web server
spawn connect web-server

# On instance:
sudo yum install -y httpd
sudo systemctl start httpd
echo "<h1>Hello from spawn!</h1>" | sudo tee /var/www/html/index.html
exit

# Test from local machine
PUBLIC_IP=$(spawn status "$INSTANCE_ID" --json | jq -r '.network.public_ip')
curl "http://$PUBLIC_IP"
# Should display: <h1>Hello from spawn!</h1>
```

## SSH Keys: Secure Access

SSH keys provide secure, password-less authentication.

### Default SSH Keys

spawn uses `~/.ssh/id_rsa` by default:

```bash
# Check if you have keys
ls -la ~/.ssh/id_rsa*

# If not found, spawn creates them automatically on first launch
spawn launch --instance-type t3.micro
```

### Custom SSH Keys

```bash
# Create project-specific key
ssh-keygen -t rsa -b 2048 -f ~/.ssh/my-project-key -N ""

# Launch with custom key
spawn launch --instance-type t3.micro --key-pair my-project-key

# Connect with custom key
spawn connect <instance-id> --key ~/.ssh/my-project-key
```

### Multiple Keys

```bash
# Development key
spawn launch --instance-type t3.micro --key-pair dev-key --name dev-instance

# Production key (more secure)
spawn launch --instance-type m7i.large --key-pair prod-key --name prod-instance
```

## Instance Lifecycle: Stop, Start, Hibernate

### States

- **Running:** Instance is running, charges accrue
- **Stopped:** Instance is stopped, no compute charges (still pay for storage)
- **Terminated:** Instance is deleted, all charges stop

### Stop Instance

```bash
# Stop instance (preserves data)
aws ec2 stop-instances --instance-ids i-xxx

# Wait for stopped state
aws ec2 wait instance-stopped --instance-ids i-xxx

# Check status
spawn status i-xxx
```

**Cost Impact:**
- Running: $0.0104/hour (t3.micro)
- Stopped: $0.0013/hour (8 GB EBS only)
- Savings: 87.5%

### Start Instance

```bash
# Start stopped instance
aws ec2 start-instances --instance-ids i-xxx

# Wait for running
aws ec2 wait instance-running --instance-ids i-xxx

# Connect
spawn connect i-xxx
```

**Note:** Public IP changes when starting stopped instance.

### Hibernate (Advanced)

Hibernation saves RAM to disk and restores on start:

```bash
# Launch with hibernation enabled
spawn launch --instance-type m7i.large --hibernate --ttl 8h

# Hibernate instance
aws ec2 stop-instances --instance-ids i-xxx --hibernate

# Resume (start)
aws ec2 start-instances --instance-ids i-xxx
```

**Benefits:**
- Faster resume (seconds vs minutes)
- Applications continue where they left off
- No startup time

**Requirements:**
- Instance family supports hibernation (m5, m6i, m7i, etc.)
- Root volume encrypted
- RAM ≤ 150 GB

## Tagging: Organize Your Instances

Tags are key-value pairs for organizing and tracking resources.

### Default spawn Tags

Every spawn instance has:
```
spawn:managed=true
spawn:root=true
spawn:created-at=2026-01-27T10:00:00Z
```

### Adding Custom Tags

```bash
# Launch with tags
spawn launch --instance-type t3.micro \
  --tags env=dev,project=website,team=backend,cost-center=engineering

# List instances by tag
spawn list --tag env=dev
spawn list --tag project=website
```

### Useful Tag Patterns

**Environment:**
```bash
--tags env=dev
--tags env=staging
--tags env=prod
```

**Project/Application:**
```bash
--tags project=website,app=frontend
--tags project=api,app=backend
```

**Team/Owner:**
```bash
--tags team=ml,owner=alice
--tags team=data,owner=bob
```

**Cost Tracking:**
```bash
--tags cost-center=engineering,budget=team-a
```

**Temporary/Permanent:**
```bash
--tags temporary=true,expires=2026-02-01
```

### Exercise: Tagged Instance Fleet

```bash
# Launch dev environment
spawn launch --instance-type t3.micro --name dev-web --tags env=dev,app=web
spawn launch --instance-type t3.micro --name dev-api --tags env=dev,app=api
spawn launch --instance-type t3.micro --name dev-db --tags env=dev,app=database

# Launch staging environment
spawn launch --instance-type t3.small --name staging-web --tags env=staging,app=web
spawn launch --instance-type t3.small --name staging-api --tags env=staging,app=api

# List by environment
echo "=== Dev Instances ==="
spawn list --tag env=dev

echo "=== Staging Instances ==="
spawn list --tag env=staging

# List web servers across all environments
echo "=== All Web Servers ==="
spawn list --tag app=web
```

## Practical Exercise: Complete Workflow

Let's put it all together with a realistic scenario.

**Scenario:** Launch a development instance for a Node.js application.

### Step 1: Launch Instance

```bash
spawn launch \
  --instance-type t3.medium \
  --name nodejs-dev \
  --ttl 8h \
  --tags env=dev,project=myapp,language=nodejs \
  --wait-for-ssh
```

### Step 2: Install Node.js

```bash
spawn connect nodejs-dev

# On instance:
# Install Node.js 20.x
curl -fsSL https://rpm.nodesource.com/setup_20.x | sudo bash -
sudo yum install -y nodejs

# Verify
node --version
npm --version

# Install common tools
npm install -g pm2 yarn

exit
```

### Step 3: Deploy Application

```bash
# Copy your code
scp -r ./myapp ec2-user@$(spawn status nodejs-dev --json | jq -r '.network.public_ip'):~/

# Connect and start app
spawn connect nodejs-dev

# On instance:
cd myapp
npm install
npm start

# Keep app running after disconnect
pm2 start npm --name myapp -- start
pm2 save
exit
```

### Step 4: Monitor

```bash
# Check instance status
spawn status nodejs-dev

# Check cost
spawn cost --instance-id $(spawn list --tag Name=nodejs-dev --quiet)

# Extend TTL if needed
spawn extend nodejs-dev 4h
```

### Step 5: Cleanup

```bash
# When done, terminate
INSTANCE_ID=$(spawn list --tag Name=nodejs-dev --quiet)
aws ec2 terminate-instances --instance-ids "$INSTANCE_ID"
```

## What You Learned

Congratulations! You now understand:

✅ Instance types and how to choose them
✅ AMIs and automatic selection
✅ Security groups and port access
✅ SSH key management
✅ Instance lifecycle (stop/start/hibernate)
✅ Tagging for organization
✅ Complete development workflow

## Best Practices

### 1. Always Set TTL

```bash
# Good
spawn launch --instance-type t3.micro --ttl 4h

# Bad (no auto-termination)
spawn launch --instance-type t3.micro
```

### 2. Use Tags Consistently

```bash
# Consistent tagging scheme
--tags env=dev,project=myapp,owner=alice,cost-center=engineering
```

### 3. Choose Right Instance Size

```bash
# Start small, scale up if needed
spawn launch --instance-type t3.micro

# Not: launch largest instance "to be safe"
```

### 4. Use Security Groups Properly

```bash
# Allow SSH from your IP only
MY_IP=$(curl -s ifconfig.me)
--cidr "$MY_IP/32"

# Not: open to world unless necessary
--cidr 0.0.0.0/0
```

## Next Steps

Continue your learning journey:

📖 **[Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md)** - Launch dozens of instances for batch processing

🛠️ **[How-To: Instance Selection](../how-to/instance-selection.md)** - Detailed guide to choosing instance types

📚 **[Command Reference](../reference/commands/launch.md)** - All launch options

## Quick Reference

```bash
# Launch with custom options
spawn launch \
  --instance-type <type> \
  --name <name> \
  --ttl <duration> \
  --tags key=value,key2=value2 \
  --ami <ami-id> \
  --security-groups <sg-id> \
  --key-pair <key-name>

# Stop/start
aws ec2 stop-instances --instance-ids <id>
aws ec2 start-instances --instance-ids <id>

# Hibernate/resume
aws ec2 stop-instances --instance-ids <id> --hibernate
aws ec2 start-instances --instance-ids <id>

# List by tags
spawn list --tag <key>=<value>
```

---

**Previous:** [← Tutorial 1: Getting Started](01-getting-started.md)
**Next:** [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md) →
