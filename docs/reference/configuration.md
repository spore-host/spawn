# Configuration Reference

spawn can be configured through configuration files, environment variables, and command-line flags.

## Configuration File Location

### Default Paths

**Linux/macOS:**
```
~/.spawn/config.yaml
```

**Windows:**
```
%USERPROFILE%\.spawn\config.yaml
```

### Custom Location

Set via environment variable:
```bash
export SPAWN_CONFIG_DIR=/opt/spawn
# Uses /opt/spawn/config.yaml
```

## Configuration File Format

spawn uses YAML format for configuration.

### Example Configuration

```yaml
# ~/.spawn/config.yaml

# Default region for operations
default_region: us-east-1

# Language for output
language: en

# Accessibility settings
no_emoji: false
accessibility_mode: false

# Default TTL for launched instances
default_ttl: 8h

# Default idle timeout
default_idle_timeout: 1h

# SSH key configuration
ssh:
  default_key: ~/.ssh/id_rsa
  default_user: ec2-user

# Instance defaults
instance:
  default_type: t3.micro
  default_ami: latest-al2023  # or specific AMI ID
  enable_hibernation: false

# Network configuration
network:
  create_vpc: true  # Auto-create VPC if needed
  assign_public_ip: true
  enable_ipv6: false

# Parameter sweep defaults
sweeps:
  max_concurrent: 5
  launch_delay: 5s
  enable_detached: true

# Cost management
cost:
  default_budget: "100"  # USD per month
  alert_threshold: 0.8   # Alert at 80% of budget

# Alerts configuration
alerts:
  default_slack_webhook: https://hooks.slack.com/services/...
  enable_email: false
  email_address: user@example.com

# Logging
logging:
  level: info  # debug, info, warn, error
  file: /var/log/spawn.log  # or empty for stderr

# Cache settings
cache:
  enabled: true
  ttl: 24h
  directory: ~/.spawn/cache
```

## Configuration Options

### Global Settings

#### default_region
**Type:** String
**Default:** From `AWS_REGION` or `~/.aws/config` or `us-east-1`
**Description:** Default AWS region for all operations.

```yaml
default_region: us-west-2
```

**Override:** `--region` flag or `AWS_REGION` environment variable.

#### language
**Type:** String
**Default:** System locale or `en`
**Allowed Values:** `en`, `es`, `fr`, `de`, `ja`, `pt`
**Description:** Language for spawn output.

```yaml
language: ja
```

**Override:** `--lang` flag or `SPAWN_LANG` environment variable.

#### no_emoji
**Type:** Boolean
**Default:** `false`
**Description:** Disable emoji in output.

```yaml
no_emoji: true
```

**Override:** `--no-emoji` flag or `SPAWN_NO_EMOJI` environment variable.

#### accessibility_mode
**Type:** Boolean
**Default:** `false`
**Description:** Enable accessibility mode (no emoji, no color, screen reader-friendly).

```yaml
accessibility_mode: true
```

**Override:** `--accessibility` flag or `SPAWN_ACCESSIBILITY` environment variable.

### Instance Defaults

#### instance.default_type
**Type:** String
**Default:** `t3.micro`
**Description:** Default instance type if not specified.

```yaml
instance:
  default_type: m7i.large
```

**Override:** `--instance-type` flag.

#### instance.default_ami
**Type:** String or `latest-al2023`
**Default:** `latest-al2023` (auto-detect)
**Description:** Default AMI to use.

```yaml
instance:
  default_ami: ami-0c55b159cbfafe1f0
  # or
  default_ami: latest-al2023  # Auto-selects based on arch/GPU
```

**Override:** `--ami` flag.

#### instance.enable_hibernation
**Type:** Boolean
**Default:** `false`
**Description:** Enable hibernation support by default.

```yaml
instance:
  enable_hibernation: true
```

**Override:** `--hibernate` flag.

### Lifecycle Defaults

#### default_ttl
**Type:** Duration
**Default:** None (no TTL)
**Description:** Default TTL for instances if not specified.

```yaml
default_ttl: 8h
```

**Override:** `--ttl` flag.
**Format:** Go duration (`30m`, `2h`, `1d`)

#### default_idle_timeout
**Type:** Duration
**Default:** None
**Description:** Default idle timeout if not specified.

```yaml
default_idle_timeout: 1h
```

**Override:** `--idle-timeout` flag.

### SSH Configuration

#### ssh.default_key
**Type:** Path
**Default:** `~/.ssh/id_rsa`
**Description:** Default SSH key to use.

```yaml
ssh:
  default_key: ~/.ssh/my-key.pem
```

**Override:** `--key` flag.

#### ssh.default_user
**Type:** String
**Default:** Detected from AMI (usually `ec2-user`)
**Description:** Default SSH user.

```yaml
ssh:
  default_user: ubuntu
```

**Override:** `--user` flag.

### Network Configuration

#### network.create_vpc
**Type:** Boolean
**Default:** `true`
**Description:** Auto-create VPC and subnet if needed.

```yaml
network:
  create_vpc: false
```

#### network.assign_public_ip
**Type:** Boolean
**Default:** `true`
**Description:** Assign public IP to instances by default.

```yaml
network:
  assign_public_ip: false
```

**Override:** `--public-ip` / `--no-public-ip` flags.

### Parameter Sweep Defaults

#### sweeps.max_concurrent
**Type:** Integer
**Default:** `5`
**Description:** Maximum concurrent instances for parameter sweeps.

```yaml
sweeps:
  max_concurrent: 10
```

**Override:** `--max-concurrent` flag.

#### sweeps.launch_delay
**Type:** Duration
**Default:** `5s`
**Description:** Delay between launching instances in sweep.

```yaml
sweeps:
  launch_delay: 10s
```

**Override:** `--launch-delay` flag.

#### sweeps.enable_detached
**Type:** Boolean
**Default:** `true`
**Description:** Enable detached mode by default for sweeps.

```yaml
sweeps:
  enable_detached: true
```

**Override:** `--detach` flag.

### Cost Management

#### cost.default_budget
**Type:** String (USD amount)
**Default:** None
**Description:** Default monthly budget.

```yaml
cost:
  default_budget: "500"
```

**Override:** `--budget` flag.

#### cost.alert_threshold
**Type:** Float (0.0-1.0)
**Default:** `0.8` (80%)
**Description:** Budget percentage threshold for alerts.

```yaml
cost:
  alert_threshold: 0.9  # Alert at 90%
```

### Alerts Configuration

#### alerts.default_slack_webhook
**Type:** URL
**Default:** None
**Description:** Default Slack webhook for alerts.

```yaml
alerts:
  default_slack_webhook: https://hooks.slack.com/services/T.../B.../XXX
```

**Override:** `--slack` flag in `spawn alerts create`.

#### alerts.enable_email
**Type:** Boolean
**Default:** `false`
**Description:** Enable email alerts.

```yaml
alerts:
  enable_email: true
  email_address: alerts@example.com
```

### Logging

#### logging.level
**Type:** String
**Default:** `info`
**Allowed Values:** `debug`, `info`, `warn`, `error`
**Description:** Logging verbosity.

```yaml
logging:
  level: debug
```

**Override:** `SPAWN_LOG_LEVEL` environment variable.

#### logging.file
**Type:** Path
**Default:** None (stderr)
**Description:** Log file path.

```yaml
logging:
  file: /var/log/spawn/spawn.log
```

**Override:** `SPAWN_LOG_FILE` environment variable.

### Cache Settings

#### cache.enabled
**Type:** Boolean
**Default:** `true`
**Description:** Enable caching of AMI IDs and instance type data.

```yaml
cache:
  enabled: false
```

#### cache.ttl
**Type:** Duration
**Default:** `24h`
**Description:** Cache entry time-to-live.

```yaml
cache:
  ttl: 1h
```

**Override:** `SPAWN_CACHE_TTL` environment variable.

#### cache.directory
**Type:** Path
**Default:** `~/.spawn/cache`
**Description:** Cache directory location.

```yaml
cache:
  directory: /var/cache/spawn
```

## Creating Configuration File

### Interactive Setup
```bash
spawn config init
# Creates ~/.spawn/config.yaml with defaults
# Prompts for common settings
```

### Manual Creation
```bash
mkdir -p ~/.spawn
cat > ~/.spawn/config.yaml <<'EOF'
default_region: us-east-1
language: en
default_ttl: 8h
EOF
```

### Copy Example
```bash
cp examples/config.yaml ~/.spawn/config.yaml
# Edit with your settings
```

## Managing Configuration

### View Current Configuration
```bash
spawn config show
# Displays effective configuration from all sources
```

### Edit Configuration
```bash
spawn config edit
# Opens config file in $EDITOR
```

### Validate Configuration
```bash
spawn config validate
# Checks syntax and values
```

### Reset to Defaults
```bash
spawn config reset
# Removes ~/.spawn/config.yaml
# Prompts for confirmation
```

## Configuration Priority

When multiple configuration sources specify the same setting, spawn uses this priority order (highest to lowest):

1. **Command-line flags** - Always highest priority
2. **Environment variables** - Override config file
3. **Configuration file** - `~/.spawn/config.yaml`
4. **AWS config** - `~/.aws/config` (for region, profile)
5. **Defaults** - Built-in defaults

### Example

```yaml
# ~/.spawn/config.yaml
default_region: us-east-1
default_ttl: 8h
```

```bash
export AWS_REGION=us-west-2

spawn launch --instance-type m7i.large --region ap-south-1 --ttl 2h
# Region: ap-south-1 (flag wins)
# TTL: 2h (flag wins)

spawn launch --instance-type m7i.large
# Region: us-west-2 (env var wins over config)
# TTL: 8h (from config)
```

## Per-Project Configuration

spawn supports per-project configuration using `.spawn.yaml` in the current directory.

```bash
# Project directory structure
my-project/
  .spawn.yaml          # Project-specific config
  params.yaml          # Parameter files
  scripts/

# .spawn.yaml
default_region: us-west-2
default_ttl: 4h
sweeps:
  max_concurrent: 20
```

**Priority:** Project config overrides user config (`~/.spawn/config.yaml`) but not command-line flags or environment variables.

## Profile-Based Configuration

For multi-account or multi-environment setups:

```yaml
# ~/.spawn/config.yaml
profiles:
  dev:
    default_region: us-west-2
    instance:
      default_type: t3.micro
    default_ttl: 2h

  prod:
    default_region: us-east-1
    instance:
      default_type: m7i.large
    default_ttl: 24h
```

```bash
export SPAWN_PROFILE=prod
spawn launch --instance-type m7i.large
# Uses prod profile settings
```

## Complete Examples

### Development Configuration

```yaml
# ~/.spawn/config.yaml - Development
default_region: us-west-2
language: en
default_ttl: 2h
default_idle_timeout: 30m

instance:
  default_type: t3.micro
  enable_hibernation: true

network:
  create_vpc: true
  assign_public_ip: true

logging:
  level: debug

sweeps:
  max_concurrent: 3
  launch_delay: 10s

cost:
  default_budget: "50"
  alert_threshold: 0.8
```

### Production Configuration

```yaml
# ~/.spawn/config.yaml - Production
default_region: us-east-1
language: en
default_ttl: 24h
default_idle_timeout: 2h

instance:
  default_type: m7i.large
  enable_hibernation: false

network:
  create_vpc: false  # Use existing VPC
  assign_public_ip: true

logging:
  level: info
  file: /var/log/spawn.log

sweeps:
  max_concurrent: 10
  launch_delay: 5s
  enable_detached: true

alerts:
  default_slack_webhook: https://hooks.slack.com/services/T.../B.../XXX
  enable_email: true
  email_address: ops@example.com

cost:
  default_budget: "1000"
  alert_threshold: 0.9
```

### ML Training Configuration

```yaml
# ~/.spawn/config.yaml - ML Training
default_region: us-east-1
language: en

instance:
  default_type: g5.xlarge
  default_ami: ami-pytorch  # Custom AMI with PyTorch
  enable_hibernation: true

default_ttl: 12h
default_idle_timeout: 1h

network:
  create_vpc: true
  assign_public_ip: true

logging:
  level: info

alerts:
  default_slack_webhook: https://hooks.slack.com/services/T.../B.../XXX

cost:
  default_budget: "500"
  alert_threshold: 0.85
```

## Troubleshooting

### Configuration not loading
```bash
# Check file exists
ls -la ~/.spawn/config.yaml

# Check syntax
spawn config validate

# View effective config
spawn config show
```

### Unexpected defaults
```bash
# Show configuration precedence
export SPAWN_DEBUG=1
spawn launch --instance-type m7i.large
# Prints which config sources are used
```

### Permission errors
```bash
# Fix permissions
chmod 600 ~/.spawn/config.yaml
chmod 700 ~/.spawn
```

## See Also
- [Environment Variables](environment-variables.md) - Environment variable reference
- [spawn Reference](README.md) - Command reference index
- [Exit Codes](exit-codes.md) - Command exit codes
