# Shell Completion for Spawn CLI

Spawn CLI includes intelligent shell completion support for bash, zsh, fish, and PowerShell. Completions provide:

- **Command completion** - Tab-complete spawn commands (launch, connect, list, stop, start, etc.)
- **Flag completion** - Tab-complete command flags (--region, --instance-type, etc.)
- **Dynamic instance ID completion** - Tab-complete running instance IDs from your AWS account
- **Dynamic DNS name completion** - Tab-complete DNS names from spawn-managed instances
- **Region completion** - Tab-complete AWS regions with descriptions
- **Instance type completion** - Tab-complete common instance types with specs

## Benefits

Shell completion is especially useful with base36 DNS names, which are longer than traditional names:
- Traditional: `my-instance.spore.host`
- Base36: `my-instance.1kpqzg2c.spore.host`

With tab completion, you can type `spawn connect my-<TAB>` to see and complete available instances.

## Quick Start

### Bash

**Install completion (one-time setup):**
```bash
# Generate completion script
spawn completion bash > ~/.spawn-completion.bash

# Add to your .bashrc
echo 'source ~/.spawn-completion.bash' >> ~/.bashrc

# Reload shell
source ~/.bashrc
```

**Alternative (system-wide):**
```bash
# Install to bash completion directory
spawn completion bash | sudo tee /etc/bash_completion.d/spawn > /dev/null
```

### Zsh

**Install completion (one-time setup):**
```bash
# Generate completion script
spawn completion zsh > ~/.spawn-completion.zsh

# Add to your .zshrc
echo 'source ~/.spawn-completion.zsh' >> ~/.zshrc

# Reload shell
source ~/.zshrc
```

**Alternative (using fpath):**
```bash
# Create completion directory if needed
mkdir -p ~/.zsh/completion

# Generate completion
spawn completion zsh > ~/.zsh/completion/_spawn

# Add to .zshrc (before compinit)
echo 'fpath=(~/.zsh/completion $fpath)' >> ~/.zshrc
echo 'autoload -Uz compinit && compinit' >> ~/.zshrc

# Reload
source ~/.zshrc
```

### Fish

**Install completion (one-time setup):**
```bash
# Generate and install completion
spawn completion fish > ~/.config/fish/completions/spawn.fish
```

Fish will automatically load completions from this location.

### PowerShell

**Install completion (one-time setup):**
```powershell
# Generate completion script
spawn completion powershell | Out-File -FilePath $PROFILE\..\spawn.ps1 -Encoding utf8

# Add to PowerShell profile
Add-Content -Path $PROFILE -Value ". $PSScriptRoot\spawn.ps1"
```

## Usage Examples

### Complete Commands
```bash
$ spawn <TAB>
completion  connect  extend  hibernate  launch  list  start  stop
```

### Complete Instance IDs
```bash
$ spawn connect <TAB>
i-0abc123def456789a  my-instance (running)
i-0xyz987fed654321b  dev-server (running)

$ spawn connect i-0a<TAB>
i-0abc123def456789a  my-instance (running)
```

### Complete Regions
```bash
$ spawn launch --region <TAB>
us-east-1      US East (N. Virginia)
us-east-2      US East (Ohio)
us-west-1      US West (N. California)
us-west-2      US West (Oregon)
eu-west-1      Europe (Ireland)
...
```

### Complete Instance Types
```bash
$ spawn launch --instance-type <TAB>
t3.micro     Burstable, 2 vCPU, 1 GB RAM
t3.small     Burstable, 2 vCPU, 2 GB RAM
m7i.large    General purpose, 2 vCPU, 8 GB RAM
c7i.xlarge   Compute optimized, 4 vCPU, 8 GB RAM
g5.xlarge    GPU (1x A10G), 4 vCPU, 16 GB RAM
...
```

### Complete DNS Names
```bash
$ spawn connect my-<TAB>
my-instance    m7i.large (i-0abc123def456789a)
my-gpu-box     g5.xlarge (i-0xyz987fed654321b)
```

## How It Works

### Static Completions
Commands, flags, regions, and instance types use static completion lists that work offline.

### Dynamic Completions
Instance IDs and DNS names query AWS in real-time (with 5-second timeout):
- Filters for `spawn:managed=true` tag
- Shows only running/stopped instances
- Includes helpful descriptions (state, type, etc.)
- Caches AWS credentials for fast subsequent completions

### Performance
- Completions are lazy-loaded (only queried when you press TAB)
- 5-second timeout prevents hanging on slow network
- AWS SDK connection pooling makes subsequent calls fast
- Works across all regions (filters by --region flag if provided)

## Troubleshooting

### Completion doesn't work
```bash
# Verify spawn is in PATH
which spawn

# Re-generate completion
spawn completion bash > ~/.spawn-completion.bash
source ~/.spawn-completion.bash
```

### Instance IDs don't show up
```bash
# Test AWS credentials
aws sts get-caller-identity

# Check for spawn-managed instances
spawn list

# Verify completion is installed
type _spawn_completion  # bash
type _spawn             # zsh
```

### Slow completions
- Dynamic completions (instance IDs, DNS names) query AWS
- First completion may take 1-2 seconds
- Subsequent completions use cached AWS connections
- 5-second timeout prevents indefinite hanging

### Permission errors
```bash
# Ensure AWS credentials have EC2 describe permissions
aws ec2 describe-instances --filters "Name=tag:spawn:managed,Values=true"
```

## Completion Features by Command

| Command | Argument Completion | Flag Completion |
|---------|---------------------|-----------------|
| `spawn launch` | - | `--region`, `--instance-type` |
| `spawn connect` | Instance ID | - |
| `spawn extend` | Instance ID | - |
| `spawn stop` | Instance ID | - |
| `spawn start` | Instance ID | - |
| `spawn hibernate` | Instance ID | - |
| `spawn list` | - | - |

## Advanced: Custom Completion

You can customize completion behavior by modifying `cmd/completion.go`:

### Add custom regions
```go
func completeRegion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	regions := []string{
		"my-region-1\tCustom Region 1",
		// Add your regions here
	}
	// ...
}
```

### Add custom instance types
```go
func completeInstanceType(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	instanceTypes := []string{
		"my-custom-type\tCustom Type Description",
		// Add your types here
	}
	// ...
}
```

### Filter instances differently
```go
func completeInstanceID(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Modify the Filters in DescribeInstancesInput
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			// Add custom filters
		},
	}
	// ...
}
```

## Uninstalling

### Bash
```bash
# Remove from .bashrc
sed -i '/spawn-completion/d' ~/.bashrc

# Remove script
rm ~/.spawn-completion.bash
```

### Zsh
```bash
# Remove from .zshrc
sed -i '/spawn-completion/d' ~/.zshrc

# Remove script
rm ~/.spawn-completion.zsh
# or
rm ~/.zsh/completion/_spawn
```

### Fish
```bash
rm ~/.config/fish/completions/spawn.fish
```

### PowerShell
```powershell
# Remove from profile
# Edit $PROFILE and remove the spawn.ps1 line

# Remove script
Remove-Item "$PSScriptRoot\spawn.ps1"
```

## See Also

- [Cobra Shell Completion](https://github.com/spf13/cobra/blob/main/shell_completions.md) - Underlying completion framework
- [AWS CLI Completion](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-completion.html) - Similar completion for AWS CLI
