# spawn config

Manage spored agent configuration on running instances.

## Synopsis

```bash
spawn config <instance-id> list
spawn config <instance-id> get <key>
spawn config <instance-id> set <key> <value>
```

## Description

The `config` command manages the spored agent configuration on running instances. It allows you to view and modify spored settings without SSH access.

**What is spored?**
- Background agent running on every spawn instance
- Handles TTL enforcement, idle detection, spot interruption monitoring
- Auto-terminates instances when TTL expires or idle
- Registers/deregisters DNS records

**Configuration Management:**
- View current spored configuration (`list`)
- Get specific configuration values (`get`)
- Update configuration at runtime (`set`)
- Changes take effect immediately (no restart required)

## Subcommands

### list

List all spored configuration keys and values.

```bash
spawn config <instance-id> list
```

### get

Get the value of a specific configuration key.

```bash
spawn config <instance-id> get <key>
```

### set

Set a configuration key to a new value.

```bash
spawn config <instance-id> set <key> <value>
```

## Arguments

**`<instance-id>`**
- Instance ID or name to configure
- Example: `i-0abc123def456789` or `my-instance`

**`<key>`**
- Configuration key to get/set
- See [Configuration Keys](#configuration-keys) for available keys

**`<value>`**
- New value for configuration key
- Type depends on key (string, int, duration, bool)

## Configuration Keys

### Core Settings

**`ttl.enabled`** (bool)
- Enable/disable TTL enforcement
- Default: `true`
- Example: `spawn config i-0abc123 set ttl.enabled false`

**`ttl.duration`** (duration)
- Time-to-live duration
- Format: `30m`, `2h`, `1d`
- Example: `spawn config i-0abc123 set ttl.duration 4h`

**`ttl.warn_minutes`** (int)
- Minutes before TTL expiration to warn users
- Default: `10`
- Example: `spawn config i-0abc123 set ttl.warn_minutes 30`

### Idle Detection

**`idle.enabled`** (bool)
- Enable/disable idle detection
- Default: `false`
- Example: `spawn config i-0abc123 set idle.enabled true`

**`idle.timeout`** (duration)
- Idle timeout duration
- Format: `30m`, `1h`, `2h`
- Example: `spawn config i-0abc123 set idle.timeout 1h`

**`idle.cpu_threshold`** (float)
- CPU utilization threshold (%)
- Below this = idle
- Default: `5.0`
- Example: `spawn config i-0abc123 set idle.cpu_threshold 10.0`

### Spot Interruption

**`spot.check_enabled`** (bool)
- Enable/disable spot interruption monitoring
- Default: `true` (for spot instances)
- Example: `spawn config i-0abc123 set spot.check_enabled false`

**`spot.check_interval`** (duration)
- How often to check for interruption warnings
- Default: `5s`
- Example: `spawn config i-0abc123 set spot.check_interval 10s`

### DNS Registration

**`dns.enabled`** (bool)
- Enable/disable DNS registration
- Default: `true`
- Example: `spawn config i-0abc123 set dns.enabled false`

**`dns.lambda_arn`** (string)
- Lambda function ARN for DNS updates
- Example: `spawn config i-0abc123 set dns.lambda_arn arn:aws:lambda:us-east-1:123456789012:function:spawn-dns-updater`

**`dns.hosted_zone_id`** (string)
- Route53 hosted zone ID
- Example: `spawn config i-0abc123 set dns.hosted_zone_id Z1234567890ABC`

### Logging

**`log.level`** (string)
- Log verbosity level
- Values: `debug`, `info`, `warn`, `error`
- Default: `info`
- Example: `spawn config i-0abc123 set log.level debug`

**`log.cloudwatch_enabled`** (bool)
- Send logs to CloudWatch
- Default: `false`
- Example: `spawn config i-0abc123 set log.cloudwatch_enabled true`

## Examples

### View All Configuration

```bash
spawn config i-0abc123def456789 list
```

**Output:**
```
Configuration for instance i-0abc123def456789:

ttl.enabled = true
ttl.duration = 2h0m0s
ttl.warn_minutes = 10
idle.enabled = false
idle.timeout = 30m0s
idle.cpu_threshold = 5.0
spot.check_enabled = true
spot.check_interval = 5s
dns.enabled = true
dns.lambda_arn = arn:aws:lambda:us-east-1:966362334030:function:spawn-dns-updater
dns.hosted_zone_id = Z1234567890ABC
log.level = info
log.cloudwatch_enabled = false
```

### Get Specific Value

```bash
# Check TTL duration
spawn config my-instance get ttl.duration
```

**Output:**
```
2h0m0s
```

```bash
# Check if idle detection enabled
spawn config my-instance get idle.enabled
```

**Output:**
```
false
```

### Extend TTL

**Extend by modifying TTL duration:**
```bash
# Get current TTL
CURRENT_TTL=$(spawn config i-0abc123 get ttl.duration)
echo "Current TTL: $CURRENT_TTL"

# Set new TTL (add 2 hours)
spawn config i-0abc123 set ttl.duration 4h

# Verify
spawn config i-0abc123 get ttl.duration
```

**Note:** Use `spawn extend` command for simpler TTL extension.

### Enable Idle Detection

**Enable idle auto-termination:**
```bash
# Enable idle detection
spawn config i-0abc123 set idle.enabled true

# Set 30-minute idle timeout
spawn config i-0abc123 set idle.timeout 30m

# Lower CPU threshold (more sensitive)
spawn config i-0abc123 set idle.cpu_threshold 2.0

# Verify settings
spawn config i-0abc123 list
```

### Disable TTL for Long-Running Job

**Disable TTL enforcement:**
```bash
# Disable TTL (instance runs until manually terminated)
spawn config i-0abc123 set ttl.enabled false

# Verify TTL disabled
spawn config i-0abc123 get ttl.enabled
# Output: false
```

**Re-enable TTL later:**
```bash
# Re-enable TTL
spawn config i-0abc123 set ttl.enabled true

# Set new duration
spawn config i-0abc123 set ttl.duration 8h
```

### Debug Spot Interruption Issues

**Enable debug logging:**
```bash
# Enable debug logs
spawn config i-0abc123 set log.level debug

# Increase check frequency
spawn config i-0abc123 set spot.check_interval 2s

# View spored logs
spawn connect i-0abc123 -c "sudo journalctl -u spored -f"
```

### Disable DNS Registration

**For instances without DNS:**
```bash
# Disable DNS registration
spawn config i-0abc123 set dns.enabled false

# Useful for:
# - Private instances without DNS needs
# - Testing without DNS overhead
# - Instances with custom DNS setup
```

### Batch Configuration

**Configure multiple instances:**
```bash
#!/bin/bash
# configure-array.sh

ARRAY_TAG="array-id=array-123"

# Get all instances in array
INSTANCES=$(spawn list --tag $ARRAY_TAG --format json | jq -r '.[].instance_id')

for INSTANCE_ID in $INSTANCES; do
  echo "Configuring $INSTANCE_ID..."

  # Enable idle detection for all
  spawn config $INSTANCE_ID set idle.enabled true
  spawn config $INSTANCE_ID set idle.timeout 1h

  echo "✓ $INSTANCE_ID configured"
done
```

## Use Cases

### 1. Extend Running Job

**Scenario:** Job taking longer than expected, need more time.

```bash
# Quick check remaining time
spawn status i-0abc123 | grep "TTL remaining"

# Extend by 4 hours
spawn config i-0abc123 set ttl.duration 4h

# Or disable TTL completely
spawn config i-0abc123 set ttl.enabled false
```

### 2. Enable Idle Termination

**Scenario:** Development instance, want to auto-terminate when idle.

```bash
# Enable idle detection
spawn config i-0abc123 set idle.enabled true
spawn config i-0abc123 set idle.timeout 15m
spawn config i-0abc123 set idle.cpu_threshold 5.0
```

**Result:** Instance terminates after 15 minutes of < 5% CPU usage.

### 3. Troubleshoot Spot Interruptions

**Scenario:** Spot instances terminating unexpectedly.

```bash
# Enable debug logging
spawn config i-0abc123 set log.level debug

# View logs in real-time
spawn connect i-0abc123 -c "sudo journalctl -u spored -f"
```

### 4. Disable Features for Testing

**Scenario:** Testing application without spored interference.

```bash
# Disable all spored features
spawn config i-0abc123 set ttl.enabled false
spawn config i-0abc123 set idle.enabled false
spawn config i-0abc123 set spot.check_enabled false
spawn config i-0abc123 set dns.enabled false
```

## Configuration Persistence

### Where Configuration is Stored

**On instance:**
- Config file: `/etc/spored/config.yaml`
- Owned by: `root:root`
- Permissions: `0600`

**In memory:**
- spored loads config on startup
- Reloads automatically when changed via `spawn config`
- No restart required

### Configuration Priority

**Precedence (highest to lowest):**
1. Runtime changes via `spawn config set`
2. Launch-time flags (`--ttl`, `--idle-timeout`)
3. Instance metadata (tags)
4. Default values

### Backup Configuration

**Export configuration:**
```bash
# Export to JSON
spawn config i-0abc123 list --json > instance-config.json

# Restore configuration
jq -r 'to_entries[] | "\(.key) \(.value)"' instance-config.json | while read KEY VALUE; do
  spawn config i-0abc123 set "$KEY" "$VALUE"
done
```

## Permissions

### Required IAM Permissions

**To run `spawn config`:**
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeInstances",
      "ssm:SendCommand",
      "ssm:GetCommandInvocation"
    ],
    "Resource": "*"
  }]
}
```

### Instance Requirements

**spored must be running:**
- Installed at `/usr/local/bin/spored`
- Running as systemd service
- Accessible via SSM or SSH

## Troubleshooting

### Config Command Times Out

**Problem:** `spawn config` hangs or times out.

**Causes:**
1. Instance not responding
2. spored not running
3. Network connectivity issues

**Solution:**
```bash
# Check instance status
spawn status i-0abc123

# Verify spored running
spawn connect i-0abc123 -c "sudo systemctl status spored"

# Restart spored if needed
spawn connect i-0abc123 -c "sudo systemctl restart spored"
```

### Configuration Not Applied

**Problem:** Configuration change doesn't take effect.

**Causes:**
1. Invalid value for key
2. Type mismatch (e.g., string for bool)
3. spored not reloading config

**Solution:**
```bash
# Verify configuration syntax
spawn config i-0abc123 get <key>

# Check spored logs for errors
spawn connect i-0abc123 -c "sudo journalctl -u spored --since '5 minutes ago'"

# Force reload
spawn connect i-0abc123 -c "sudo systemctl reload spored"
```

### Unknown Configuration Key

**Problem:** "Unknown configuration key: xyz"

**Solution:**
- Check [Configuration Keys](#configuration-keys) for valid keys
- Verify spelling and case (keys are case-sensitive)
- Use `spawn config <instance-id> list` to see all available keys

## Related Commands

- **[spawn extend](extend.md)** - Simpler way to extend TTL
- **[spawn status](status.md)** - View instance status and remaining TTL
- **[spawn connect](connect.md)** - SSH to instance for manual config editing
- **[spawn launch](launch.md)** - Set configuration at launch time

## See Also

- [Reference: Configuration](../configuration.md) - Config file format
- [How-To: Debugging](../../how-to/debugging.md) - Troubleshooting guide
- [Tutorial 7: Monitoring & Alerts](../../tutorials/07-monitoring-alerts.md) - Monitoring setup
