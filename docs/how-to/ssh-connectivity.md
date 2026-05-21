# How-To: SSH & Connectivity

Advanced SSH configuration and connectivity troubleshooting.

## SSH Key Management

### Generate SSH Keys

```bash
# Standard RSA key (recommended)
ssh-keygen -t rsa -b 4096 -f ~/.ssh/spawn-key -N ""

# ED25519 key (newer, smaller)
ssh-keygen -t ed25519 -f ~/.ssh/spawn-ed25519 -N ""
```

### Use Custom SSH Key

```bash
# Launch with custom key
spawn launch --instance-type t3.micro --key-pair my-custom-key

# Connect with custom key
spawn connect <instance-id> --key ~/.ssh/my-custom-key
```

### Multiple Keys for Different Environments

```bash
# Development key
ssh-keygen -t rsa -b 4096 -f ~/.ssh/spawn-dev -N ""
spawn launch --key-pair spawn-dev --tags env=dev

# Production key (more secure, requires passphrase)
ssh-keygen -t rsa -b 4096 -f ~/.ssh/spawn-prod
spawn launch --key-pair spawn-prod --tags env=prod
```

---

## SSH Config File

### Basic SSH Config

Create `~/.ssh/config`:

```
# spawn instances
Host spawn-*
    User ec2-user
    IdentityFile ~/.ssh/spawn-key
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    ServerAliveInterval 60
    ServerAliveCountMax 3

# spawn development
Host spawn-dev-*
    User ec2-user
    IdentityFile ~/.ssh/spawn-dev
    ForwardAgent yes

# spawn production
Host spawn-prod-*
    User ec2-user
    IdentityFile ~/.ssh/spawn-prod
    ForwardAgent no
```

### Connect Using Config

```bash
# Add hostname to config dynamically
PUBLIC_IP=$(spawn status <instance-id> --json | jq -r '.network.public_ip')

cat >> ~/.ssh/config << EOF
Host spawn-work
    HostName $PUBLIC_IP
    User ec2-user
    IdentityFile ~/.ssh/spawn-key
EOF

# Now connect using alias
ssh spawn-work
```

---

## Port Forwarding

### Local Port Forwarding

Forward remote service to local port.

**Example: Access Jupyter notebook running on instance**

```bash
# On instance, Jupyter runs on port 8888
spawn connect <instance-id> -L 8888:localhost:8888

# Now open http://localhost:8888 on your local machine
```

**Example: Forward database**

```bash
# PostgreSQL on instance port 5432 → local port 5432
spawn connect <instance-id> -L 5432:localhost:5432

# Connect locally
psql -h localhost -p 5432 -U postgres
```

### Remote Port Forwarding

Forward local service to instance.

**Example: Instance accesses service on your laptop**

```bash
# Your laptop runs service on port 3000
# Make it accessible from instance on port 3000
spawn connect <instance-id> -R 3000:localhost:3000

# On instance:
curl http://localhost:3000  # Reaches your laptop's service
```

### Dynamic Port Forwarding (SOCKS Proxy)

```bash
# Create SOCKS proxy on local port 1080
spawn connect <instance-id> -D 1080

# Configure browser to use SOCKS proxy localhost:1080
# All browser traffic routes through instance
```

---

## Bastion Hosts

### SSH Through Bastion

**Scenario:** Instances in private subnet, bastion in public subnet.

**Method 1: ProxyJump (SSH >= 7.3)**

```bash
# ~/.ssh/config
Host bastion
    HostName bastion-public-ip
    User ec2-user
    IdentityFile ~/.ssh/bastion-key

Host spawn-private-*
    User ec2-user
    IdentityFile ~/.ssh/spawn-key
    ProxyJump bastion
```

**Connect:**
```bash
ssh spawn-private-10.0.1.100
# Automatically jumps through bastion
```

**Method 2: ProxyCommand (older SSH)**

```bash
# ~/.ssh/config
Host spawn-private-*
    User ec2-user
    IdentityFile ~/.ssh/spawn-key
    ProxyCommand ssh -W %h:%p bastion
```

**Method 3: spawn connect with bastion**

```bash
# Connect through bastion
spawn connect <instance-id> --bastion <bastion-id>

# Or with bastion IP
spawn connect <instance-id> --bastion-ip 54.123.45.67 --bastion-key ~/.ssh/bastion-key
```

---

## AWS Session Manager (SSM)

### Enable Session Manager

**Launch with SSM:**
```bash
spawn launch --instance-type t3.micro --enable-ssm
```

**Connect via Session Manager:**
```bash
# No SSH keys needed, uses IAM auth
spawn connect <instance-id> --ssm

# Or directly with AWS CLI
aws ssm start-session --target <instance-id>
```

### Benefits of Session Manager

**Pros:**
- No SSH keys needed
- No open ports (no security group changes)
- Full audit logging
- IAM-based access control
- Works with private instances (no bastion needed)

**Cons:**
- Requires SSM agent (pre-installed on Amazon Linux 2023)
- Requires IAM permissions
- Slightly slower than direct SSH

### Port Forwarding via SSM

```bash
# Forward remote port 8888 to local port 8888
aws ssm start-session \
  --target <instance-id> \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8888"],"localPortNumber":["8888"]}'

# Now access http://localhost:8888
```

---

## VPN Integration

### Connect spawn Instances to Corporate VPN

**Scenario:** Instances need to access internal corporate resources.

**Option 1: OpenVPN**

```bash
# Launch instance
spawn launch --instance-type t3.medium --name vpn-client

# Connect and install OpenVPN
spawn connect vpn-client

# On instance:
sudo yum install -y openvpn

# Copy VPN config from laptop
exit
scp ~/corporate.ovpn ec2-user@<public-ip>:~/

# Connect again and start VPN
spawn connect vpn-client
sudo openvpn --config corporate.ovpn --daemon

# Verify VPN connection
ip addr show tun0
ping internal-server.corp.local
```

**Option 2: AWS Client VPN**

```bash
# Setup AWS Client VPN endpoint (one-time setup)
# See: https://docs.aws.amazon.com/vpn/latest/clientvpn-admin/

# Launch instance with VPN endpoint
spawn launch --instance-type t3.micro --vpn-endpoint cvpn-endpoint-xxx
```

### Site-to-Site VPN

**Connect entire VPC to corporate network:**

```bash
# Create VPN connection (AWS Console or CLI)
# Then all spawn instances in that VPC can access corporate resources

spawn launch --instance-type t3.micro --vpc vpc-xxx
# Instance automatically has access to corporate network
```

---

## SSH Agent Forwarding

### Enable Agent Forwarding

**Allow using local SSH keys on remote instance.**

```bash
# ~/.ssh/config
Host spawn-*
    ForwardAgent yes
```

**Use case: Clone private Git repo from instance**

```bash
# Start SSH agent locally
eval $(ssh-agent)
ssh-add ~/.ssh/github-key

# Connect with agent forwarding
spawn connect <instance-id>

# On instance, can clone private repos
git clone git@github.com:myorg/private-repo.git
# Uses your local SSH key automatically
```

**Security warning:** Only use agent forwarding on trusted instances.

---

## Tmux/Screen for Persistent Sessions

### Using Tmux

**Keep sessions alive after disconnect.**

```bash
# Connect to instance
spawn connect <instance-id>

# Start tmux
tmux new -s work

# Run long-running command
python train.py

# Detach: Ctrl+B then D

# Disconnect from instance
exit

# Later, reconnect
spawn connect <instance-id>

# Reattach to session
tmux attach -t work
# Your program is still running!
```

### Using Screen

```bash
# Start screen session
screen -S work

# Run command
python train.py

# Detach: Ctrl+A then D

# Reattach later
screen -r work
```

---

## Troubleshooting SSH

### Connection Timeout

**Error:**
```
ssh: connect to host 54.123.45.67 port 22: Connection timed out
```

**Causes & Solutions:**

**1. Instance still initializing**
```bash
# Wait for SSH to be ready
spawn connect <instance-id> --wait
```

**2. Security group blocks SSH**
```bash
# Check security group
INSTANCE_ID=<instance-id>
SG_ID=$(aws ec2 describe-instances --instance-ids $INSTANCE_ID \
  --query 'Reservations[0].Instances[0].SecurityGroups[0].GroupId' --output text)

# Check rules
aws ec2 describe-security-groups --group-ids $SG_ID

# Add SSH rule from your IP
MY_IP=$(curl -s ifconfig.me)
aws ec2 authorize-security-group-ingress \
  --group-id $SG_ID \
  --protocol tcp \
  --port 22 \
  --cidr $MY_IP/32
```

**3. Wrong IP address**
```bash
# Get correct public IP
spawn status <instance-id> --json | jq -r '.network.public_ip'
```

**4. Network ACL blocks traffic**
```bash
# Check VPC network ACLs
# (Less common, usually security groups are the issue)
```

---

### Connection Refused

**Error:**
```
ssh: connect to host 54.123.45.67 port 22: Connection refused
```

**Causes:**

**1. SSH daemon not running**
```bash
# Connect via Session Manager
spawn connect <instance-id> --ssm

# Check SSH service
sudo systemctl status sshd

# Start if stopped
sudo systemctl start sshd
```

**2. SSH listening on different port**
```bash
# Check SSH config
sudo cat /etc/ssh/sshd_config | grep Port

# If Port 2222, connect with:
ssh -p 2222 ec2-user@<ip>
```

---

### Permission Denied (publickey)

**Error:**
```
Permission denied (publickey).
```

**Causes & Solutions:**

**1. Wrong SSH key**
```bash
# Use correct key
spawn connect <instance-id> --key ~/.ssh/correct-key

# Or check which key pair instance was launched with
spawn status <instance-id> --json | jq -r '.key_pair'
```

**2. Wrong username**
```bash
# Amazon Linux 2023: ec2-user
ssh ec2-user@<ip>

# Ubuntu: ubuntu
ssh ubuntu@<ip>

# Debian: admin
ssh admin@<ip>
```

**3. Key permissions wrong**
```bash
# SSH keys must be mode 600
chmod 600 ~/.ssh/my-key

# .ssh directory must be 700
chmod 700 ~/.ssh
```

**4. Public key not in authorized_keys**
```bash
# Connect via Session Manager
spawn connect <instance-id> --ssm

# Check authorized_keys
cat ~/.ssh/authorized_keys

# Add key manually
echo "<your-public-key>" >> ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys
```

---

### Host Key Verification Failed

**Error:**
```
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
```

**Cause:** IP address reused from terminated instance with different host key.

**Solution:**
```bash
# Remove old host key
ssh-keygen -R 54.123.45.67

# Or disable host key checking (less secure)
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ec2-user@<ip>

# Or in ~/.ssh/config:
Host spawn-*
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
```

---

### Slow SSH Connection

**Problem:** SSH connection takes 30+ seconds to establish.

**Causes & Solutions:**

**1. DNS reverse lookup**
```bash
# Disable DNS lookup in sshd_config
sudo vi /etc/ssh/sshd_config
# Add: UseDNS no
sudo systemctl restart sshd
```

**2. GSSAPI authentication**
```bash
# Disable in local ssh config
# ~/.ssh/config
Host *
    GSSAPIAuthentication no
```

**3. Slow network**
```bash
# Check latency
ping <instance-ip>

# Use compression for slow connections
ssh -C ec2-user@<ip>
```

---

## SCP and RSYNC

### Copy Files with SCP

```bash
# Copy to instance
scp file.txt ec2-user@<instance-ip>:/home/ec2-user/

# Copy from instance
scp ec2-user@<instance-ip>:/path/to/file.txt ./

# Copy directory recursively
scp -r mydir/ ec2-user@<instance-ip>:/path/

# With custom SSH key
scp -i ~/.ssh/my-key file.txt ec2-user@<ip>:/path/
```

### Sync with Rsync

```bash
# Sync directory to instance (faster than scp for large dirs)
rsync -avz -e "ssh -i ~/.ssh/my-key" mydir/ ec2-user@<ip>:/path/

# Sync from instance
rsync -avz -e "ssh -i ~/.ssh/my-key" ec2-user@<ip>:/path/ ./local-dir/

# Exclude files
rsync -avz --exclude='*.log' --exclude='.git' mydir/ ec2-user@<ip>:/path/

# Show progress
rsync -avzP mydir/ ec2-user@<ip>:/path/
```

---

## SSH Multiplexing

### Speed Up Repeated Connections

**Problem:** Opening multiple SSH connections to same instance is slow.

**Solution: Connection reuse**

```bash
# ~/.ssh/config
Host spawn-*
    ControlMaster auto
    ControlPath ~/.ssh/control-%r@%h:%p
    ControlPersist 10m
```

**Effect:**
- First connection: Normal speed
- Subsequent connections: Instant (reuses existing connection)
- Connections close 10 minutes after last use

---

## Jump Hosts and Multi-Hop SSH

### Two-Hop SSH

```bash
# Connect to private instance through bastion
ssh -J bastion-user@bastion-ip private-user@private-ip

# Or with specific keys
ssh -J bastion-user@bastion-ip -i ~/.ssh/private-key private-user@private-ip
```

### Three-Hop SSH

```bash
# Laptop → Bastion → Jump Host → Private Instance
ssh -J bastion@ip1,jumphost@ip2 ec2-user@private-ip
```

---

## Batch Operations

### Run Command on Multiple Instances

```bash
#!/bin/bash
# run-on-all.sh

# Get all instance IDs from sweep
INSTANCES=$(spawn list --tag sweep:id=sweep-xxx --format json | jq -r '.[].instance_id')

# Run command on each
for instance in $INSTANCES; do
  echo "Running on $instance..."
  spawn connect $instance -c "hostname && uptime"
done
```

### Parallel SSH (pssh)

```bash
# Install parallel-ssh
pip install parallel-ssh

# Create hosts file
spawn list --format json | jq -r '.[].network.public_ip' > hosts.txt

# Run command on all hosts in parallel
parallel-ssh -h hosts.txt -l ec2-user -i "uptime"
```

---

## SSH Security Best Practices

### 1. Use Strong Keys

```bash
# RSA 4096-bit or ED25519
ssh-keygen -t rsa -b 4096
ssh-keygen -t ed25519
```

### 2. Protect Private Keys

```bash
# Private key permissions: 600
chmod 600 ~/.ssh/id_rsa

# Add passphrase to keys (more secure)
ssh-keygen -p -f ~/.ssh/id_rsa
```

### 3. Disable Password Authentication

```bash
# On instance: /etc/ssh/sshd_config
PasswordAuthentication no
ChallengeResponseAuthentication no
```

### 4. Use SSH Certificate Authority

```bash
# For organizations, use SSH CA for centralized key management
# See: https://smallstep.com/blog/use-ssh-certificates/
```

### 5. Rotate Keys Regularly

```bash
# Generate new key
ssh-keygen -t rsa -b 4096 -f ~/.ssh/spawn-key-2026

# Launch new instances with new key
spawn launch --key-pair spawn-key-2026

# Decommission old key after migration
```

---

## See Also

- [Tutorial 1: Getting Started](../tutorials/01-getting-started.md) - SSH basics
- [Tutorial 2: Your First Instance](../tutorials/02-first-instance.md) - SSH keys
- [How-To: Debugging](debugging.md) - SSH troubleshooting
- [spawn connect](../reference/commands/connect.md) - Connect command reference
