# spawn connect / spawn ssh

Connect to spawn-managed instances via SSH. Both commands are aliases and work identically.

## Synopsis

```bash
spawn connect <instance-id-or-name> [flags]
spawn ssh <instance-id-or-name> [flags]
```

## Description

Establish SSH connection to a spawn-managed EC2 instance. Supports connection by instance ID or instance name (Name tag), automatic SSH key discovery, and fallback to AWS Session Manager for instances without public IPs.

## Arguments

### instance-id-or-name
**Type:** String
**Required:** Yes
**Description:** EC2 instance ID (i-xxxxx) or instance name (Name tag value).

```bash
# By instance ID
spawn connect i-0123456789abcdef0

# By instance name
spawn connect my-dev-instance
```

## Flags

### SSH Configuration

#### --user
**Type:** String
**Default:** Auto-detected from AMI (usually `ec2-user` for Amazon Linux, `ubuntu` for Ubuntu)
**Description:** SSH username for connection.

```bash
spawn connect i-1234567890 --user ubuntu
spawn ssh my-instance --user ec2-user
```

**Common Users by AMI:**
- Amazon Linux 2023: `ec2-user`
- Ubuntu: `ubuntu`
- Debian: `admin`
- RHEL: `ec2-user`
- CentOS: `centos`

#### --key, -i
**Type:** Path
**Default:** Auto-detected from `~/.ssh/` directory
**Description:** SSH private key file path.

```bash
spawn connect i-1234567890 --key ~/.ssh/my-key.pem
spawn ssh my-instance -i my-project-key  # Auto-finds in ~/.ssh/
```

**Key Search Order:**
1. Exact path: `~/.ssh/my-key`
2. With `.pem` extension: `~/.ssh/my-key.pem`
3. With `.key` extension: `~/.ssh/my-key.key`
4. Default keys: `~/.ssh/id_rsa`, `~/.ssh/id_ed25519`, `~/.ssh/id_ecdsa`, `~/.ssh/id_dsa`

#### --port
**Type:** Integer
**Default:** `22`
**Description:** SSH port number.

```bash
spawn connect i-1234567890 --port 2222
```

### Connection Method

#### --session-manager
**Type:** Boolean
**Default:** `false` (auto-enabled if no public IP)
**Description:** Force AWS Session Manager connection (bypasses public IP).

```bash
spawn connect i-1234567890 --session-manager
```

**Requirements:**
- Instance must have SSM agent running
- Instance IAM role must have `AmazonSSMManagedInstanceCore` policy
- Local machine must have `session-manager-plugin` installed

**Advantages:**
- Works without public IP
- No need for SSH key
- Centralized access logging via CloudTrail
- No inbound firewall rules required

**Installation:**
```bash
# macOS
brew install --cask session-manager-plugin

# Linux
curl "https://s3.amazonaws.com/session-manager-downloads/plugin/latest/linux_64bit/session-manager-plugin.rpm" -o "session-manager-plugin.rpm"
sudo yum install -y session-manager-plugin.rpm
```

### Additional Options

#### --command, -c
**Type:** String
**Default:** None (interactive shell)
**Description:** Execute command and exit (non-interactive).

```bash
spawn connect i-1234567890 --command "uptime"
spawn ssh my-instance -c "df -h"
```

#### --ssh-options
**Type:** String
**Default:** None
**Description:** Additional SSH options to pass through.

```bash
spawn connect i-1234567890 --ssh-options "-o StrictHostKeyChecking=no"
spawn ssh my-instance --ssh-options "-L 8080:localhost:8080"  # Port forwarding
```

#### --wait
**Type:** Boolean
**Default:** `false`
**Description:** Wait for instance to be running and SSH to be available.

```bash
spawn connect i-1234567890 --wait
# Useful right after launch
```

#### --timeout
**Type:** Duration
**Default:** `5m`
**Description:** Timeout for `--wait` flag.

```bash
spawn connect i-1234567890 --wait --timeout 10m
```

## Connection Flow

1. **Resolve Instance**
   - If argument starts with `i-`, treat as instance ID
   - Otherwise, look up instance by Name tag

2. **Wait for Running State**
   - If `--wait`, poll until instance is running
   - Otherwise, check state immediately

3. **Get Connection Info**
   - Determine public IP or decide on Session Manager
   - Determine SSH user (from AMI or `--user`)
   - Find SSH key (auto-detect or from `--key`)

4. **Establish Connection**
   - If public IP available: Direct SSH connection
   - If no public IP: AWS Session Manager connection
   - If `--session-manager` forced: Session Manager regardless

5. **Interactive Shell or Command Execution**
   - Interactive: Start shell session
   - `--command`: Execute and exit

## Examples

### Basic Connection
```bash
# By instance ID
spawn connect i-0123456789abcdef0

# By instance name
spawn connect my-dev-instance
```

### Custom User
```bash
# Ubuntu instance
spawn connect ubuntu-instance --user ubuntu

# Amazon Linux
spawn ssh my-instance --user ec2-user
```

### Custom SSH Key
```bash
spawn connect i-1234567890 --key ~/.ssh/my-project-key.pem

# Auto-finds key in ~/.ssh/
spawn ssh my-instance --key my-project-key
```

### Execute Single Command
```bash
# Check uptime
spawn connect i-1234567890 --command "uptime"

# Get disk usage
spawn ssh my-instance -c "df -h"

# Copy file via SSH
spawn ssh my-instance -c "cat /var/log/app.log" > local-app.log
```

### Session Manager (Private Instance)
```bash
# Force Session Manager
spawn connect private-instance --session-manager

# Auto-uses Session Manager if no public IP
spawn connect private-instance
```

### Port Forwarding
```bash
# Forward local port 8080 to remote port 8080
spawn connect i-1234567890 --ssh-options "-L 8080:localhost:8080"

# Multiple forwards
spawn ssh my-instance --ssh-options "-L 8080:localhost:8080 -L 5432:localhost:5432"
```

### Wait for Instance to be Ready
```bash
# Launch and connect
INSTANCE=$(spawn launch --instance-type m7i.large --quiet)
spawn connect "$INSTANCE" --wait

# Or in one line
spawn connect $(spawn launch --instance-type m7i.large --quiet) --wait
```

### Automation Examples
```bash
# Get first running instance and connect
spawn connect $(spawn list --state running --quiet | head -1)

# Connect to specific job array instance
spawn connect $(spawn list --tag spawn:job-array-index=0 --quiet)

# Execute command on all running instances
for instance in $(spawn list --state running --quiet); do
    echo "Checking $instance..."
    spawn connect "$instance" -c "hostname && uptime"
done
```

## SSH Config Integration

spawn respects `~/.ssh/config` settings:

```
# ~/.ssh/config
Host *.spore.host
    User ec2-user
    IdentityFile ~/.ssh/my-key.pem
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    ServerAliveInterval 60
    ServerAliveCountMax 3

Host i-*
    ProxyCommand aws ec2-instance-connect send-ssh-public-key --instance-id %h --availability-zone %p --instance-os-user %r
```

## Troubleshooting

### Connection Refused
```bash
# Check instance state
spawn status i-1234567890

# Wait for SSH to be ready
spawn connect i-1234567890 --wait
```

### Permission Denied (Public Key)
```bash
# Wrong SSH user
spawn connect i-1234567890 --user ubuntu  # Try different user

# Wrong SSH key
spawn connect i-1234567890 --key ~/.ssh/other-key.pem

# Check which key was used
spawn connect i-1234567890 --verbose  # Shows SSH command
```

### Timeout
```bash
# Increase timeout
spawn connect i-1234567890 --wait --timeout 10m

# Check security group allows SSH (port 22)
aws ec2 describe-security-groups --group-ids sg-xxxxx

# Use Session Manager as fallback
spawn connect i-1234567890 --session-manager
```

### No Public IP
```bash
# Use Session Manager
spawn connect i-1234567890 --session-manager

# Check if public IP was assigned
spawn list --format json | jq '.[] | select(.instance_id == "i-1234567890") | .public_ip'
```

### Session Manager Not Working
```bash
# Check SSM agent status on instance
spawn connect i-1234567890 -c "systemctl status amazon-ssm-agent"

# Check IAM role has SSM permissions
aws iam get-role --role-name spored-instance-role | jq '.Role.AssumeRolePolicyDocument'

# Install session-manager-plugin locally
brew install --cask session-manager-plugin  # macOS
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | SSH connection successful, session exited normally |
| 1 | Connection failed (instance not reachable, SSH timeout) |
| 2 | Invalid arguments (instance ID/name not provided) |
| 3 | Instance not found (no instance with given ID/name) |
| 4 | Instance not running (instance is stopped or terminated) |
| 5 | SSH key not found (key file doesn't exist) |
| 255 | SSH connection error (standard SSH exit code) |

## Performance

- **Direct SSH:** ~500ms connection time (public IP)
- **Session Manager:** ~2-3s connection time (AWS API overhead)
- **Key discovery:** Caches results for 5 minutes

## Security Considerations

### SSH Keys
- Never share private keys
- Use separate keys per environment (dev/prod)
- Rotate keys regularly
- Use `ssh-agent` for key management

### Session Manager Advantages
- No SSH key required (uses AWS IAM authentication)
- All sessions logged in CloudTrail
- No direct internet exposure
- Centralized access control via IAM policies

### Hardening SSH
```bash
# Disable password authentication (in user-data)
echo "PasswordAuthentication no" | sudo tee -a /etc/ssh/sshd_config
sudo systemctl restart sshd

# Use SSH certificates instead of keys
# Configure CertificateAuthority in sshd_config
```

## See Also
- [spawn launch](launch.md) - Launch instances
- [spawn list](list.md) - List instances
- [spawn extend](extend.md) - Extend instance TTL
- [spawn status](status.md) - Check instance status
- [Configuration](../configuration.md) - SSH configuration options
- [Troubleshooting](../../troubleshooting/connectivity.md) - Connection issues
