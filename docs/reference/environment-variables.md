# Environment Variables Reference

spawn respects several environment variables for configuration and behavior control.

## AWS Configuration

### AWS_PROFILE
**Type:** String
**Default:** `default`
**Description:** AWS profile name to use for credentials and configuration.

```bash
export AWS_PROFILE=spore-host-dev
spawn launch --instance-type m7i.large
```

**Use Case:** Multi-account AWS environments, separate dev/prod credentials.

### AWS_REGION
**Type:** String
**Default:** From AWS config or `us-east-1`
**Description:** Default AWS region for operations.

```bash
export AWS_REGION=us-west-2
spawn launch --instance-type m7i.large
# Launches in us-west-2 by default
```

**Note:** Command-line `--region` flag takes precedence.

### AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
**Type:** String
**Default:** None
**Description:** AWS credentials for API access.

```bash
export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
spawn launch --instance-type m7i.large
```

**Security Warning:** Avoid using these variables. Prefer AWS profiles or IAM roles.

## spawn Configuration

### SPAWN_LANG
**Type:** String
**Default:** System locale or `en`
**Allowed Values:** `en`, `es`, `fr`, `de`, `ja`, `pt`
**Description:** Language for spawn output.

```bash
export SPAWN_LANG=es
spawn launch --instance-type m7i.large
# Output in Spanish
```

**Priority Order:**
1. `--lang` flag (highest)
2. `SPAWN_LANG` environment variable
3. Config file `~/.spawn/config.yaml`
4. System locale (`LANG`, `LC_ALL`)
5. Default to English

### SPAWN_NO_EMOJI
**Type:** Boolean (any non-empty value)
**Default:** Not set
**Description:** Disable emoji in output.

```bash
export SPAWN_NO_EMOJI=1
spawn list
# Output without emoji
```

**Equivalent:** `spawn --no-emoji <command>`

### SPAWN_ACCESSIBILITY
**Type:** Boolean (any non-empty value)
**Default:** Not set
**Description:** Enable accessibility mode (no emoji, no color, screen reader-friendly).

```bash
export SPAWN_ACCESSIBILITY=1
spawn launch --instance-type m7i.large
# Output optimized for screen readers
```

**Equivalent:** `spawn --accessibility <command>`

### SPAWN_CONFIG_DIR
**Type:** Path
**Default:** `~/.spawn` (Linux/macOS), `%USERPROFILE%\.spawn` (Windows)
**Description:** Directory for spawn configuration and cache.

```bash
export SPAWN_CONFIG_DIR=/opt/spawn/config
spawn launch --instance-type m7i.large
# Uses /opt/spawn/config/config.yaml
```

**Contents:**
- `config.yaml` - User configuration
- `cache/` - Cached AMI IDs, instance type info
- `state/` - State files for detached sweeps

### SPAWN_CACHE_TTL
**Type:** Duration
**Default:** `24h`
**Description:** How long to cache AMI IDs and instance type data.

```bash
export SPAWN_CACHE_TTL=1h
spawn launch --instance-type m7i.large
# Cache expires after 1 hour instead of 24
```

**Format:** Go duration (`30m`, `2h`, `1d`)

## SSH Configuration

### SSH_AUTH_SOCK
**Type:** Path
**Default:** Set by SSH agent
**Description:** SSH agent socket path. spawn uses this for SSH key authentication.

```bash
eval $(ssh-agent)
export SSH_AUTH_SOCK=$SSH_AUTH_SOCK
spawn connect i-1234567890
```

### HOME
**Type:** Path
**Default:** User home directory
**Description:** Used to locate `~/.ssh/` directory for SSH keys.

```bash
export HOME=/custom/home
spawn launch --instance-type m7i.large
# Looks for SSH keys in /custom/home/.ssh/
```

## Logging and Debugging

### SPAWN_DEBUG
**Type:** Boolean (any non-empty value)
**Default:** Not set
**Description:** Enable debug logging.

```bash
export SPAWN_DEBUG=1
spawn launch --instance-type m7i.large
# Prints detailed debug information
```

**Output Includes:**
- AWS API calls and responses
- Cache hits/misses
- Decision logic (AMI selection, region choice)
- Timing information

### SPAWN_LOG_LEVEL
**Type:** String
**Default:** `info`
**Allowed Values:** `debug`, `info`, `warn`, `error`
**Description:** Logging verbosity level.

```bash
export SPAWN_LOG_LEVEL=debug
spawn launch --instance-type m7i.large
```

### SPAWN_LOG_FILE
**Type:** Path
**Default:** None (logs to stderr)
**Description:** File path for log output.

```bash
export SPAWN_LOG_FILE=/var/log/spawn/spawn.log
spawn launch --instance-type m7i.large
# Logs written to file instead of stderr
```

## Lambda and Detached Mode

### WEBHOOK_KMS_KEY_ID
**Type:** String (KMS key ID or alias)
**Default:** None
**Description:** KMS key for encrypting webhook URLs in Lambda alert handler.

```bash
export WEBHOOK_KMS_KEY_ID=alias/spawn-webhook-encryption
# Used in lambda/alert-handler/main.go
```

**Context:** Lambda environment variable, not CLI.

### SWEEP_ORCHESTRATOR_TIMEOUT
**Type:** Duration
**Default:** `10m`
**Description:** Timeout for sweep orchestrator Lambda function.

```bash
export SWEEP_ORCHESTRATOR_TIMEOUT=15m
# Extends Lambda timeout for large sweeps
```

**Context:** Lambda configuration.

## spored Agent (On EC2 Instances)

These variables are set automatically on instances via user-data when spored is installed.

### SPORED_CONFIG_FILE
**Type:** Path
**Default:** `/etc/spored/config.json`
**Description:** spored configuration file location.

### SPORED_LOG_FILE
**Type:** Path
**Default:** `/var/log/spored.log`
**Description:** spored log file location.

### SPORED_CHECK_INTERVAL
**Type:** Duration
**Default:** `1m`
**Description:** How often spored checks TTL, idle status, completion signals.

```bash
export SPORED_CHECK_INTERVAL=30s
systemctl restart spored
# Checks every 30 seconds instead of 1 minute
```

## System Variables

### LANG, LC_ALL
**Type:** String
**Default:** System default
**Description:** System locale. Used for language detection if `SPAWN_LANG` not set.

```bash
export LANG=ja_JP.UTF-8
spawn list
# Output in Japanese (if SPAWN_LANG not set)
```

### EDITOR
**Type:** Path
**Default:** `vi` or `nano`
**Description:** Text editor for interactive config editing.

```bash
export EDITOR=vim
spawn config edit
# Opens config in vim
```

### PAGER
**Type:** Path
**Default:** `less` or `more`
**Description:** Pager for long output.

```bash
export PAGER=less
spawn list --verbose
# Pages output with less
```

## CI/CD Environments

### CI
**Type:** Boolean (any non-empty value)
**Default:** Not set
**Description:** Indicates running in CI environment. spawn disables interactive prompts.

```bash
export CI=true
spawn launch --instance-type m7i.large
# No interactive prompts, fails on missing required flags
```

**Set automatically by:** GitHub Actions, GitLab CI, CircleCI, etc.

### GITHUB_ACTIONS, GITLAB_CI, CIRCLECI
**Type:** Boolean (any non-empty value)
**Default:** Not set
**Description:** Specific CI platform indicators. spawn detects and adapts behavior.

```bash
# GitHub Actions
export GITHUB_ACTIONS=true
spawn launch --instance-type m7i.large
# Optimized output for GitHub Actions logs
```

## Complete Example: Production Configuration

```bash
# AWS Configuration
export AWS_PROFILE=prod
export AWS_REGION=us-east-1

# spawn Configuration
export SPAWN_LANG=en
export SPAWN_CONFIG_DIR=/opt/spawn
export SPAWN_CACHE_TTL=1h

# Logging
export SPAWN_LOG_LEVEL=info
export SPAWN_LOG_FILE=/var/log/spawn.log

# Accessibility (if needed)
# export SPAWN_ACCESSIBILITY=1

# Launch instance
spawn launch --instance-type m7i.large --ttl 8h
```

## Complete Example: Development Configuration

```bash
# AWS Configuration
export AWS_PROFILE=dev
export AWS_REGION=us-west-2

# spawn Configuration
export SPAWN_LANG=en
export SPAWN_DEBUG=1
export SPAWN_LOG_LEVEL=debug

# Launch instance with debug output
spawn launch --instance-type t3.micro --ttl 2h
```

## Priority and Precedence

When multiple configuration sources exist, spawn uses this priority order (highest to lowest):

1. **Command-line flags** - Always takes precedence
2. **Environment variables** - Documented in this file
3. **Config file** - `~/.spawn/config.yaml` (or `$SPAWN_CONFIG_DIR/config.yaml`)
4. **AWS config** - `~/.aws/config`
5. **System defaults** - Hardcoded defaults

### Example

```bash
# Config file: ~/.spawn/config.yaml
# region: us-east-1

export AWS_REGION=us-west-2

spawn launch --instance-type m7i.large --region ap-south-1
# Result: Launches in ap-south-1 (flag wins)

spawn launch --instance-type m7i.large
# Result: Launches in us-west-2 (env var wins over config file)
```

## Validation and Troubleshooting

### Check current configuration
```bash
spawn config show
# Displays effective configuration from all sources
```

### Test environment variables
```bash
env | grep SPAWN
env | grep AWS
```

### Debug configuration precedence
```bash
export SPAWN_DEBUG=1
spawn launch --instance-type m7i.large
# Shows which config sources are used
```

## See Also
- [Configuration](configuration.md) - Config file format and options
- [spawn Reference](README.md) - Command reference index
- [Exit Codes](exit-codes.md) - Command exit codes
