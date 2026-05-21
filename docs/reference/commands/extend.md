# spawn extend

Extend the TTL (time-to-live) for running instances to prevent automatic termination.

## Synopsis

```bash
spawn extend <instance-id-or-name> <duration> [flags]
```

## Description

Add time to an instance's TTL to prevent automatic termination. The extension is added to the current TTL value (not from current time), and the spored agent is notified to update its internal timer.

## Arguments

### instance-id-or-name
**Type:** String
**Required:** Yes
**Description:** EC2 instance ID (i-xxxxx) or instance name (Name tag value).

```bash
spawn extend i-0123456789abcdef0 2h
spawn extend my-instance 1d
```

### duration
**Type:** Duration
**Required:** Yes
**Description:** Duration to add to current TTL.

```bash
spawn extend i-1234567890 30m      # Add 30 minutes
spawn extend i-1234567890 2h       # Add 2 hours
spawn extend i-1234567890 1d       # Add 1 day
spawn extend i-1234567890 3h30m    # Add 3 hours 30 minutes
spawn extend i-1234567890 1d12h    # Add 1 day 12 hours
```

**Format:** Go duration (`30m`, `2h`, `1d`, `3h30m`)
**Units:**
- `s` - Seconds
- `m` - Minutes
- `h` - Hours
- `d` - Days (24 hours)

**Valid Examples:**
- `30m`, `2h`, `1d`, `3h30m`, `1d12h`, `2d4h30m`

**Invalid Examples:**
- `2h 30m` (no spaces)
- `2.5h` (no decimals, use `2h30m`)
- `1w` (no weeks, use `7d`)

## Flags

### --region
**Type:** String
**Default:** Auto-detected or from config
**Description:** AWS region where instance is located.

```bash
spawn extend i-1234567890 2h --region us-west-2
```

### --force
**Type:** Boolean
**Default:** `false`
**Description:** Extend even if instance doesn't have TTL set.

```bash
# Instance launched without --ttl
spawn extend i-1234567890 8h --force
# Sets TTL to 8h from now
```

### --absolute
**Type:** Boolean
**Default:** `false`
**Description:** Set TTL to absolute time instead of adding duration.

```bash
# Set TTL to 8 hours from now (not 8 hours added to current)
spawn extend i-1234567890 8h --absolute
```

### --job-array-name
**Type:** String
**Default:** None
**Description:** Extend all instances in a job array.

```bash
# Extend all instances in job array
spawn extend --job-array-name training-sweep 2h

# Equivalent to:
for id in $(spawn list --tag spawn:job-array-name=training-sweep --quiet); do
    spawn extend "$id" 2h
done
```

## How It Works

1. **Read Current TTL**
   - Reads `spawn:ttl` tag from instance
   - Parses current TTL duration

2. **Calculate New TTL**
   - Adds extension duration to current TTL
   - Or sets absolute TTL if `--absolute`

3. **Update Tag**
   - Updates `spawn:ttl` tag with new duration
   - Tag format: `8h`, `1d12h`, etc.

4. **Notify spored Agent**
   - Sends signal to spored via SSH (if accessible)
   - Or uses Systems Manager to update config
   - spored reloads configuration and updates timer

5. **Confirmation**
   - Displays current, extension, and new TTL
   - Shows new expiration time

## Examples

### Extend Single Instance
```bash
# Extend by 2 hours
spawn extend i-0123456789abcdef0 2h

# Extend by name
spawn extend my-training-job 4h

# Extend by 1 day
spawn extend long-running-job 1d
```

### Extend Job Array
```bash
# Extend all instances in job array
spawn extend --job-array-name hyperparam-sweep 2h

# Extend specific instance in array
spawn extend $(spawn list --tag spawn:job-array-index=0 --quiet) 1h
```

### Extend Multiple Instances
```bash
# Extend all running instances
for instance in $(spawn list --state running --quiet); do
    spawn extend "$instance" 2h
done

# Extend all instances with specific tag
for instance in $(spawn list --tag env=prod --quiet); do
    spawn extend "$instance" 4h
done
```

### Set Absolute TTL
```bash
# Set TTL to 8 hours from now (ignores current TTL)
spawn extend i-1234567890 8h --absolute
```

### Extend Instance Without TTL
```bash
# Instance launched without --ttl
spawn extend i-1234567890 4h --force
# Sets TTL to 4h from now
```

### Check Before Extending
```bash
# Check current TTL
spawn status i-1234567890 | grep TTL

# Extend if less than 1 hour remaining
# (logic in script)
TTL=$(spawn status i-1234567890 --json | jq -r '.ttl_remaining')
if [[ "$TTL" < "1h" ]]; then
    spawn extend i-1234567890 2h
fi
```

## Output

### Standard Output
```
Extending TTL for instance i-0123456789abcdef0...
  ✓ Current TTL: 4 hours
  ✓ Extension: 2 hours
  ✓ New TTL: 6 hours

Instance will now run for approximately 6 hours from now.
Expires at: 2026-01-27 16:30:00 PST
```

### JSON Output
```bash
spawn extend i-1234567890 2h --json
```

```json
{
  "instance_id": "i-0123456789abcdef0",
  "previous_ttl": "4h",
  "extension": "2h",
  "new_ttl": "6h",
  "expires_at": "2026-01-27T16:30:00-08:00",
  "success": true
}
```

### Quiet Output
```bash
spawn extend i-1234567890 2h --quiet
# No output, only exit code
```

## Behavior Notes

### TTL Calculation
- **Default Mode (Additive):** Adds duration to current TTL
  - Current TTL: 4h, Extension: 2h → New TTL: 6h

- **Absolute Mode:** Sets TTL from current time
  - Extension: 8h → New TTL: 8h from now (ignores current)

### spored Agent Notification
- **With SSH Access:** Sends SIGHUP to spored (instant reload)
- **Without SSH Access:** Uses Systems Manager (may take up to 1 minute)
- **Fallback:** spored checks tags every 5 minutes automatically

### Edge Cases
- **Instance without TTL:** Fails unless `--force` specified
- **Instance stopped:** Can extend, but TTL won't count down until restarted
- **Instance terminating:** Cannot extend (too late)

## Automation Examples

### Extend Before TTL Expires
```bash
#!/bin/bash
# Monitor and extend if TTL < 30 minutes

INSTANCE_ID="i-0123456789abcdef0"

while true; do
    TTL_REMAINING=$(spawn status "$INSTANCE_ID" --json | jq -r '.ttl_remaining')

    # Convert to minutes (simplified)
    if [[ "$TTL_REMAINING" =~ ^([0-9]+)m$ ]]; then
        MINUTES=${BASH_REMATCH[1]}
        if [ "$MINUTES" -lt 30 ]; then
            echo "TTL low ($MINUTES min), extending by 2h"
            spawn extend "$INSTANCE_ID" 2h
        fi
    fi

    sleep 300  # Check every 5 minutes
done
```

### Extend Job Array Based on Progress
```bash
#!/bin/bash
# Extend job array if jobs are still running

JOB_ARRAY_NAME="training-sweep"

# Check if any jobs still running
RUNNING=$(spawn list --tag spawn:job-array-name="$JOB_ARRAY_NAME" --state running --quiet | wc -l)

if [ "$RUNNING" -gt 0 ]; then
    echo "Jobs still running, extending by 4 hours"
    spawn extend --job-array-name "$JOB_ARRAY_NAME" 4h
else
    echo "All jobs complete, no extension needed"
fi
```

### Conditional Extension in CI/CD
```yaml
# GitHub Actions example
- name: Extend instance if tests take long
  run: |
    INSTANCE_ID=${{ steps.launch.outputs.instance_id }}

    # Run tests in background
    spawn connect "$INSTANCE_ID" -c "pytest tests/ &"

    # Wait 1 hour
    sleep 3600

    # Check if tests still running
    if spawn connect "$INSTANCE_ID" -c "pgrep pytest" > /dev/null; then
      echo "Tests still running, extending TTL"
      spawn extend "$INSTANCE_ID" 2h
    fi
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | TTL extended successfully |
| 1 | Extension failed (AWS API error, instance not found) |
| 2 | Invalid duration (malformed duration string) |
| 3 | Instance not managed by spawn (missing `spawn:managed=true` tag) |
| 4 | Instance has no TTL (use `--force` to set one) |

## Troubleshooting

### "Instance has no TTL"
```bash
# Error
spawn extend i-1234567890 2h
# Error: Instance has no TTL set

# Solution: Use --force to set TTL
spawn extend i-1234567890 8h --force
```

### "Instance not managed by spawn"
```bash
# Error
spawn extend i-1234567890 2h
# Error: Instance not managed by spawn

# Solution: Only works on instances launched by spawn
# Check tags
aws ec2 describe-tags --filters "Name=resource-id,Values=i-1234567890"
```

### Extension Not Taking Effect
```bash
# Check spored status on instance
spawn connect i-1234567890 -c "systemctl status spored"

# Check spored logs
spawn connect i-1234567890 -c "journalctl -u spored -n 50"

# Manually reload spored
spawn connect i-1234567890 -c "sudo systemctl reload spored"
```

### Invalid Duration Format
```bash
# Error
spawn extend i-1234567890 "2 hours"
# Error: Invalid duration format

# Solution: Use Go duration format
spawn extend i-1234567890 2h  # Correct
```

## Security Considerations

- **Permission Required:** `ec2:CreateTags` to update instance tags
- **SSH Access:** May require SSH access to notify spored immediately
- **Systems Manager:** Alternative notification method via SSM

## Performance

- **Tag Update:** ~100-200ms (AWS API call)
- **spored Notification:**
  - Via SSH: Instant (~500ms total)
  - Via SSM: Up to 1 minute
  - Fallback: Up to 5 minutes (next tag check)

## See Also
- [spawn launch](launch.md) - Launch instances with TTL
- [spawn status](status.md) - Check remaining TTL
- [spawn list](list.md) - List instances
- [Configuration](../configuration.md) - Default TTL settings
