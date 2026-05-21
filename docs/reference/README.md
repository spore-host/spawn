# spawn Reference Documentation

Complete technical reference for all spawn commands, configuration, and behavior.

## Quick Navigation

### Commands
- [launch](commands/launch.md) - Launch EC2 instances
- [list](commands/list.md) - List spawn-managed instances
- [connect/ssh](commands/connect.md) - Connect via SSH
- [extend](commands/extend.md) - Extend instance TTL
- [status](commands/status.md) - Check instance or sweep status
- [cancel](commands/cancel.md) - Cancel running sweeps
- [alerts](commands/alerts.md) - Manage alerts and notifications
- [schedule](commands/schedule.md) - Schedule parameter sweeps
- [queue](commands/queue.md) - Manage batch job queues
- [cost](commands/cost.md) - Track costs and spending
- [create-ami](commands/create-ami.md) - Create custom AMIs
- [list-amis](commands/list-amis.md) - List spawn-managed AMIs
- [stage](commands/stage.md) - Manage multi-region data staging
- [slurm](commands/slurm.md) - Run Slurm batch scripts
- [resume](commands/resume.md) - Resume interrupted sweeps
- [collect-results](commands/collect-results.md) - Collect sweep results
- [list-sweeps](commands/list-sweeps.md) - List parameter sweeps
- [dns](commands/dns.md) - Manage DNS records
- [fsx](commands/fsx.md) - FSx filesystem management

### Configuration & Environment
- [Configuration](configuration.md) - Config files and options
- [Environment Variables](environment-variables.md) - All environment variables
- [Exit Codes](exit-codes.md) - Command exit codes
- [Parameter Files](parameter-files.md) - Parameter sweep files
- [Queue Configs](queue-configs.md) - Batch queue configuration
- [IAM Policies](iam-policies.md) - IAM permissions and policies

## Command Categories

### Instance Management
Launch, connect, and manage individual EC2 instances.

- `spawn launch` - Launch instances with comprehensive options
- `spawn list` - List and filter instances
- `spawn connect` / `spawn ssh` - SSH connection
- `spawn extend` - Extend TTL to prevent termination
- `spawn status` - Check instance status

### Parameter Sweeps
Orchestrate hundreds of instances for hyperparameter tuning and batch processing.

- `spawn launch --param-file` - Launch parameter sweeps
- `spawn status --sweep-id` - Check sweep progress
- `spawn cancel --sweep-id` - Cancel running sweep
- `spawn resume --sweep-id` - Resume interrupted sweep
- `spawn collect-results` - Aggregate sweep results
- `spawn list-sweeps` - List all sweeps

### Job Scheduling
Schedule sweeps for future execution via EventBridge.

- `spawn schedule create` - Schedule one-time or recurring sweeps
- `spawn schedule list` - List schedules
- `spawn schedule describe` - View schedule details
- `spawn schedule pause` / `resume` - Control schedules
- `spawn schedule cancel` - Delete schedules

### Batch Queues
Sequential job execution with dependency management and retry logic.

- `spawn launch --batch-queue` - Launch queue execution
- `spawn queue status` - Monitor queue progress
- `spawn queue results` - Download completed results

### Monitoring & Alerts
Track costs, configure notifications, and monitor operations.

- `spawn alerts create` - Configure Slack/webhook alerts
- `spawn alerts list` - List alert configurations
- `spawn alerts delete` - Remove alerts
- `spawn cost` - Track spending and breakdown

### AMI Management
Create and manage custom AMIs for reusable software stacks.

- `spawn create-ami` - Create AMI from instance
- `spawn list-amis` - List spawn-managed AMIs

### Data Staging
Multi-region data distribution for cost-efficient parameter sweeps.

- `spawn stage create` - Stage data to multiple regions
- `spawn stage list` - List staging jobs
- `spawn stage status` - Check staging progress

### Cloud Migration
Tools for migrating HPC workloads from on-premises to AWS.

- `spawn slurm` - Run Slurm batch scripts on AWS

## Documentation Conventions

### Notation
- `<required>` - Required argument
- `[optional]` - Optional argument
- `--flag` - Command flag
- `string|int|duration|bool` - Argument types
- `...` - Repeatable argument

### Duration Format
Durations use Go's time format:
- `30m` - 30 minutes
- `2h` - 2 hours
- `1d` - 1 day (24 hours)
- `3h30m` - 3 hours 30 minutes
- `1d12h` - 1 day 12 hours

### JSON Output
Most commands support `--json` or `--format json` for machine-readable output:
```bash
spawn list --format json | jq '.[] | select(.State == "running")'
```

### Exit Codes
All commands follow standard Unix conventions:
- `0` - Success
- `1` - General error
- `2` - Usage error (invalid flags/arguments)
- `3+` - Command-specific errors

See [Exit Codes](exit-codes.md) for complete reference.

## Common Patterns

### Filtering and Selection
Many commands support filtering:
```bash
# Filter by region
spawn list --region us-east-1

# Filter by state
spawn list --state running

# Filter by instance family
spawn list --family m7i

# Filter by tag
spawn list --tag env=prod

# Combine filters
spawn list --region us-east-1 --state running --family m7i
```

### Output Formats
```bash
# Default table output
spawn list

# JSON for automation
spawn list --format json

# YAML
spawn list --format yaml

# Quiet mode (IDs only)
spawn list --quiet
```

### Internationalization
spawn supports 6 languages:
```bash
spawn --lang es launch     # Spanish
spawn --lang ja list       # Japanese
spawn --lang fr connect    # French
```

See [Configuration](configuration.md) for language settings.

### Accessibility
```bash
# Disable emoji only
spawn --no-emoji list

# Full accessibility mode
spawn --accessibility list
```

## Getting Help

### Command Help
```bash
# General help
spawn --help

# Command-specific help
spawn launch --help
spawn alerts --help

# Subcommand help
spawn schedule create --help
```

### Version Information
```bash
spawn --version
```

### Documentation Paths
- User Guides: [../../README.md](../../README.md)
- Tutorials: [../tutorials/](../tutorials/)
- How-To Guides: [../how-to/](../how-to/)
- Explanation: [../explanation/](../explanation/)
- Troubleshooting: [../troubleshooting/](../troubleshooting/)

## See Also
- [spawn README](../../README.md) - Overview and quick start
- [CHANGELOG.md](../../CHANGELOG.md) - Version history
- [TROUBLESHOOTING.md](../../TROUBLESHOOTING.md) - Common issues
- [IAM_PERMISSIONS.md](../../IAM_PERMISSIONS.md) - Required AWS permissions
