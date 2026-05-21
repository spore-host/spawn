# Exit Codes Reference

spawn follows standard Unix conventions for exit codes. All commands return 0 on success and non-zero on failure.

## Standard Exit Codes

| Code | Name | Description | When Used |
|------|------|-------------|-----------|
| 0 | Success | Command completed successfully | All commands on success |
| 1 | General Error | Command failed | AWS API errors, network errors, general failures |
| 2 | Usage Error | Invalid command usage | Invalid flags, missing required arguments, conflicting options |

## Command-Specific Exit Codes

### spawn launch

| Code | Description | Example |
|------|-------------|---------|
| 0 | Instance launched successfully | Instance reached running state |
| 1 | Launch failed | AWS API error, capacity issue, network error |
| 2 | Invalid arguments | Missing `--instance-type`, invalid `--ttl` format |
| 3 | Capacity error | No capacity in requested AZ |
| 4 | Permission denied | Insufficient IAM permissions |

### spawn connect

| Code | Description | Example |
|------|-------------|---------|
| 0 | SSH connection successful | Connected and shell exited normally |
| 1 | Connection failed | Instance not reachable, SSH timeout |
| 2 | Invalid arguments | Instance ID/name not provided |
| 3 | Instance not found | No instance with given ID/name |
| 4 | Instance not running | Instance is stopped or terminated |
| 5 | SSH key not found | Key file doesn't exist |

### spawn list

| Code | Description | Example |
|------|-------------|---------|
| 0 | List completed | Results displayed (even if empty) |
| 1 | API error | AWS API failure, network error |
| 2 | Invalid filter | Invalid `--state` value, malformed tag filter |

### spawn extend

| Code | Description | Example |
|------|-------------|---------|
| 0 | TTL extended successfully | Tag updated, spored notified |
| 1 | Extension failed | AWS API error, instance not found |
| 2 | Invalid duration | Malformed duration string |
| 3 | Instance not managed | Instance missing `spawn:managed=true` tag |

### spawn status

| Code | Description | Example |
|------|-------------|---------|
| 0 | Status retrieved | Instance or sweep status displayed |
| 1 | Status check failed | AWS API error, network error |
| 2 | Invalid arguments | Missing sweep ID or instance ID |
| 3 | Resource not found | Instance or sweep doesn't exist |

### spawn cancel

| Code | Description | Example |
|------|-------------|---------|
| 0 | Cancellation successful | Sweep cancelled, instances terminated |
| 1 | Cancellation failed | AWS API error, partial failure |
| 2 | Invalid sweep ID | Malformed or missing sweep ID |
| 3 | Sweep not found | Sweep doesn't exist |
| 4 | Sweep already complete | Cannot cancel completed sweep |

### spawn alerts

| Code | Description | Example |
|------|-------------|---------|
| 0 | Alert operation successful | Alert created, listed, or deleted |
| 1 | Operation failed | DynamoDB error, invalid webhook URL |
| 2 | Invalid arguments | Missing required flags, invalid alert type |
| 3 | Alert not found | Alert ID doesn't exist |

### spawn schedule

| Code | Description | Example |
|------|-------------|---------|
| 0 | Schedule operation successful | Schedule created, paused, resumed, or cancelled |
| 1 | Operation failed | EventBridge error, S3 upload failed |
| 2 | Invalid schedule expression | Malformed cron or at expression |
| 3 | Schedule not found | Schedule ID doesn't exist |

### spawn queue

| Code | Description | Example |
|------|-------------|---------|
| 0 | Queue operation successful | Status displayed, results downloaded |
| 1 | Operation failed | S3 error, instance not running |
| 2 | Invalid arguments | Missing queue ID or instance ID |
| 3 | Queue not found | Queue doesn't exist or instance has no queue |

### spawn create-ami

| Code | Description | Example |
|------|-------------|---------|
| 0 | AMI created successfully | AMI registered and available |
| 1 | AMI creation failed | EC2 error, snapshot failure |
| 2 | Invalid arguments | Missing instance ID, invalid tags |
| 3 | Instance not found | Instance doesn't exist |
| 4 | Instance not stopped | Instance must be stopped for no-reboot=false |

### spawn cost

| Code | Description | Example |
|------|-------------|---------|
| 0 | Cost report generated | Costs displayed successfully |
| 1 | Cost calculation failed | Missing pricing data, API error |
| 2 | Invalid time range | Invalid date format, end before start |

## Exit Code Handling in Scripts

### Bash

Check exit code:
```bash
spawn launch --instance-type m7i.large
if [ $? -eq 0 ]; then
    echo "Launch successful"
else
    echo "Launch failed with code $?"
fi
```

Exit on error:
```bash
set -e  # Exit on any non-zero exit code
spawn launch --instance-type m7i.large
spawn connect $(spawn list --quiet | head -1)
```

Conditional logic:
```bash
spawn extend i-1234567890 2h
case $? in
    0)
        echo "Extended successfully"
        ;;
    3)
        echo "Instance not managed by spawn"
        ;;
    *)
        echo "Extension failed"
        ;;
esac
```

### Python

```python
import subprocess
import sys

result = subprocess.run(
    ["spawn", "launch", "--instance-type", "m7i.large"],
    capture_output=True
)

if result.returncode == 0:
    print("Launch successful")
elif result.returncode == 3:
    print("No capacity available")
else:
    print(f"Launch failed: {result.returncode}")
    sys.exit(1)
```

### Makefile

```makefile
.PHONY: launch
launch:
	spawn launch --instance-type m7i.large
	# Make will stop on non-zero exit

.PHONY: launch-ignore-errors
launch-ignore-errors:
	-spawn launch --instance-type m7i.large
	# Prefix with - to ignore errors
```

### GitHub Actions

```yaml
- name: Launch instance
  id: launch
  run: spawn launch --instance-type m7i.large
  continue-on-error: false  # Fail workflow on non-zero exit

- name: Connect
  if: steps.launch.outcome == 'success'
  run: spawn connect $(spawn list --quiet | head -1)
```

## Debugging Exit Codes

### Check exit code
```bash
spawn launch --instance-type m7i.large
echo "Exit code: $?"
```

### Verbose error output
```bash
spawn launch --instance-type m7i.large --verbose 2>&1
```

### Capture stderr
```bash
spawn launch --instance-type m7i.large 2>error.log
cat error.log
```

## Common Exit Code Scenarios

### Capacity Issues (Exit 3)
```bash
spawn launch --instance-type p5.48xlarge --region us-east-1
# Exit code: 3 (no capacity)

# Solution: Try different region or AZ
spawn launch --instance-type p5.48xlarge --region us-west-2
```

### Permission Errors (Exit 4)
```bash
spawn launch --instance-type m7i.large
# Exit code: 4 (insufficient permissions)

# Solution: Check IAM permissions
./scripts/validate-permissions.sh
```

### Invalid Arguments (Exit 2)
```bash
spawn extend i-1234567890 invalid-duration
# Exit code: 2 (invalid duration format)

# Solution: Use valid duration
spawn extend i-1234567890 2h
```

### Resource Not Found (Exit 3)
```bash
spawn connect nonexistent-instance
# Exit code: 3 (instance not found)

# Solution: Check instance ID
spawn list
spawn connect i-0123456789abcdef
```

## Best Practices

### 1. Always Check Exit Codes in Scripts
```bash
#!/bin/bash
set -e  # Exit on first error

spawn launch --instance-type m7i.large
INSTANCE_ID=$(spawn list --quiet | head -1)
spawn extend "$INSTANCE_ID" 4h
```

### 2. Provide Meaningful Error Messages
```bash
spawn launch --instance-type m7i.large || {
    echo "ERROR: Failed to launch instance"
    echo "Check AWS permissions and capacity"
    exit 1
}
```

### 3. Use Exit Codes for Conditional Logic
```bash
if spawn status --sweep-id sweep-123 > /dev/null 2>&1; then
    echo "Sweep still running"
else
    echo "Sweep complete or failed"
fi
```

### 4. Log Exit Codes for Debugging
```bash
spawn launch --instance-type m7i.large
EXIT_CODE=$?
echo "$(date): spawn launch exited with code $EXIT_CODE" >> deploy.log
exit $EXIT_CODE
```

## See Also
- [spawn Reference](README.md) - Command reference index
- [Configuration](configuration.md) - Configuration options
- [Troubleshooting](../troubleshooting/common-errors.md) - Common errors and fixes
