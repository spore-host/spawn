# Spot Instance Support for Spawn

## Overview

Add AWS Spot Instance support to spawn, enabling 70-90% cost savings for fault-tolerant workloads. Spot instances use spare EC2 capacity at reduced prices but can be interrupted with 2-minute warning when AWS needs capacity back.

## Use Cases

**Ideal for:**
- Batch processing (can checkpoint and resume)
- ML training with checkpointing
- CI/CD build workers
- Data processing pipelines
- Development/testing environments
- Render farms
- Scientific computing with fault tolerance

**Not ideal for:**
- Production web servers (unless using ASG with mixed instances)
- Databases without replication
- Long-running stateful processes without checkpointing
- Workloads requiring guaranteed uptime

## User Interface

### Basic Spot Usage

```bash
# Launch spot instance (default: no fallback)
spawn launch --instance-type m7i.large --spot

# With max price (default: on-demand price)
spawn launch --instance-type m7i.large --spot --spot-max-price 0.05

# With on-demand fallback
spawn launch --instance-type m7i.large --spot --spot-fallback

# Multiple instance types for better availability
spawn launch --instance-types m7i.large,m6i.large,m5.large --spot
```

### Job Arrays with Spot

```bash
# Launch spot job array
spawn launch --count 8 --job-array-name training --spot

# Mixed: some spot, some on-demand
spawn launch --count 8 --job-array-name training --spot --spot-count 6

# With interruption handling
spawn launch --count 8 --job-array-name training --spot \
  --on-interruption checkpoint \
  --checkpoint-script ./save_checkpoint.sh
```

### Spot-Specific Flags

```
--spot                     Launch as spot instance
--spot-max-price PRICE     Maximum price per hour (default: on-demand price)
--spot-fallback            Fall back to on-demand if spot unavailable
--spot-count N             For job arrays: N spot instances, rest on-demand
--on-interruption ACTION   Action on interruption: checkpoint, hibernate, terminate
--checkpoint-script PATH   Script to run on interruption (before termination)
```

## Architecture

### Spot Request Flow

```
1. User runs: spawn launch --spot
2. CLI creates spot instance request
3. AWS fulfills request (or fails if capacity unavailable)
4. Instance launches with spot tags
5. Spored monitors interruption notices (http://169.254.169.254/latest/meta-data/spot/instance-action)
6. On interruption: Execute handler, cleanup, terminate gracefully
```

### Interruption Handling

**Two-Minute Warning Flow:**

```
1. AWS sends interruption notice (2 minutes before termination)
2. Spored detects notice via metadata endpoint
3. Spored executes user-defined checkpoint script (if provided)
4. Spored sends notification (if configured)
5. Spored performs graceful shutdown
6. AWS terminates instance after 2 minutes
```

## Implementation Details

### 1. Launch Command Changes

**File:** `cmd/launch.go`

**New Flags:**
```go
var (
	launchSpot              bool
	launchSpotMaxPrice      string
	launchSpotFallback      bool
	launchSpotCount         int
	launchOnInterruption    string
	launchCheckpointScript  string
	launchInstanceTypes     []string // Multiple types for better availability
)

func init() {
	launchCmd.Flags().BoolVar(&launchSpot, "spot", false, "Launch as spot instance")
	launchCmd.Flags().StringVar(&launchSpotMaxPrice, "spot-max-price", "", "Maximum spot price per hour (default: on-demand price)")
	launchCmd.Flags().BoolVar(&launchSpotFallback, "spot-fallback", false, "Fall back to on-demand if spot unavailable")
	launchCmd.Flags().IntVar(&launchSpotCount, "spot-count", 0, "For job arrays: number of spot instances (rest on-demand)")
	launchCmd.Flags().StringVar(&launchOnInterruption, "on-interruption", "terminate", "Action on interruption: checkpoint, hibernate, terminate")
	launchCmd.Flags().StringVar(&launchCheckpointScript, "checkpoint-script", "", "Script to run on interruption")
	launchCmd.Flags().StringSliceVar(&launchInstanceTypes, "instance-types", []string{}, "Multiple instance types (comma-separated) for better spot availability")
}
```

**Launch Logic:**
```go
func runLaunch(cmd *cobra.Command, args []string) error {
	// If --spot and spot request fails and no --spot-fallback, error
	// If --spot-fallback, try on-demand

	if launchSpot {
		instanceID, err := launchSpotInstance(ctx, config)
		if err != nil && launchSpotFallback {
			fmt.Fprintf(os.Stderr, "Spot unavailable, falling back to on-demand...\n")
			instanceID, err = launchOnDemandInstance(ctx, config)
		}
		if err != nil {
			return fmt.Errorf("failed to launch instance: %w", err)
		}
	} else {
		instanceID, err := launchOnDemandInstance(ctx, config)
	}
}
```

### 2. AWS Client Changes

**File:** `pkg/aws/client.go`

**Extend LaunchConfig:**
```go
type LaunchConfig struct {
	// Existing fields...

	// Spot configuration
	Spot                bool
	SpotMaxPrice        string
	SpotInstanceTypes   []string // For diversification
	OnInterruption      string   // checkpoint, hibernate, terminate
	CheckpointScript    string
}
```

**New Launch Function:**
```go
// LaunchSpotInstance launches a spot instance
func (c *Client) LaunchSpotInstance(ctx context.Context, config LaunchConfig) (string, error) {
	ec2Client := ec2.NewFromConfig(c.cfg)

	// Build instance market options
	marketOptions := &types.InstanceMarketOptionsRequest{
		MarketType: types.MarketTypeSpot,
		SpotOptions: &types.SpotMarketOptions{
			SpotInstanceType: types.SpotInstanceTypeOneTime, // one-time request
		},
	}

	// Set max price if specified (default: on-demand price)
	if config.SpotMaxPrice != "" {
		marketOptions.SpotOptions.MaxPrice = aws.String(config.SpotMaxPrice)
	}

	// Add interruption behavior
	if config.OnInterruption == "hibernate" {
		marketOptions.SpotOptions.InstanceInterruptionBehavior = types.InstanceInterruptionBehaviorHibernate
	} else {
		marketOptions.SpotOptions.InstanceInterruptionBehavior = types.InstanceInterruptionBehaviorTerminate
	}

	// If multiple instance types specified, use fleet request for better availability
	if len(config.SpotInstanceTypes) > 1 {
		return c.launchSpotFleet(ctx, config)
	}

	// Single instance type: use RunInstances with spot options
	result, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:               aws.String(config.AMI),
		InstanceType:          types.InstanceType(config.InstanceType),
		KeyName:               aws.String(config.KeyName),
		SecurityGroupIds:      config.SecurityGroups,
		SubnetId:              aws.String(config.SubnetID),
		UserData:              aws.String(config.UserData),
		InstanceMarketOptions: marketOptions, // <-- Spot configuration
		TagSpecifications:     buildTagSpecs(config),
		MinCount:              aws.Int32(1),
		MaxCount:              aws.Int32(1),
	})

	if err != nil {
		return "", fmt.Errorf("failed to launch spot instance: %w", err)
	}

	return *result.Instances[0].InstanceId, nil
}

// launchSpotFleet uses EC2 Fleet for multiple instance type fallbacks
func (c *Client) launchSpotFleet(ctx context.Context, config LaunchConfig) (string, error) {
	// Build launch template overrides for each instance type
	overrides := make([]types.FleetLaunchTemplateOverridesRequest, len(config.SpotInstanceTypes))
	for i, instanceType := range config.SpotInstanceTypes {
		overrides[i] = types.FleetLaunchTemplateOverridesRequest{
			InstanceType: types.InstanceType(instanceType),
		}
	}

	// Create fleet request with lowest-price allocation strategy
	// Returns first fulfilled instance
}
```

### 3. Spot-Specific Tags

**Added to all spot instances:**
```go
tags := map[string]string{
	"spawn:spot":                "true",
	"spawn:spot-max-price":      config.SpotMaxPrice,
	"spawn:on-interruption":     config.OnInterruption,
	"spawn:checkpoint-script":   config.CheckpointScript,
}
```

### 4. Spored Agent - Interruption Monitoring

**File:** `pkg/agent/agent.go`

**New Goroutine:**
```go
func (a *Agent) Start(ctx context.Context) error {
	// Existing goroutines...

	// Add spot interruption monitoring
	if a.isSpotInstance() {
		go a.monitorSpotInterruption(ctx)
	}
}

func (a *Agent) isSpotInstance() bool {
	// Check instance metadata
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/instance-life-cycle")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return string(body) == "spot"
}

func (a *Agent) monitorSpotInterruption(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check for interruption notice
			if action := a.checkInterruptionNotice(); action != nil {
				a.handleInterruption(ctx, action)
				return
			}
		}
	}
}

func (a *Agent) checkInterruptionNotice() *InterruptionAction {
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/spot/instance-action")
	if err != nil || resp.StatusCode == 404 {
		return nil // No interruption notice
	}
	defer resp.Body.Close()

	// Parse interruption notice
	var action InterruptionAction
	json.NewDecoder(resp.Body).Decode(&action)
	return &action
}

type InterruptionAction struct {
	Action string    `json:"action"` // "terminate" or "hibernate"
	Time   time.Time `json:"time"`   // When interruption will occur
}

func (a *Agent) handleInterruption(ctx context.Context, action *InterruptionAction) {
	a.logger.Warn("Spot instance interruption notice received",
		"action", action.Action,
		"time", action.Time,
		"remaining", time.Until(action.Time))

	// Get interruption handler from tags
	onInterruption := a.getTag("spawn:on-interruption")
	checkpointScript := a.getTag("spawn:checkpoint-script")

	switch onInterruption {
	case "checkpoint":
		if checkpointScript != "" {
			a.logger.Info("Running checkpoint script", "script", checkpointScript)

			// Download and execute checkpoint script
			if err := a.executeCheckpoint(ctx, checkpointScript); err != nil {
				a.logger.Error("Checkpoint failed", "error", err)
			}
		}

		// Send notification
		a.sendInterruptionNotification(action)

	case "hibernate":
		// AWS will hibernate automatically
		a.logger.Info("Instance will hibernate")
		a.sendInterruptionNotification(action)

	case "terminate":
		fallthrough
	default:
		// Graceful shutdown
		a.logger.Info("Performing graceful shutdown")
		a.sendInterruptionNotification(action)
		a.cleanup(ctx)
	}
}

func (a *Agent) executeCheckpoint(ctx context.Context, scriptURL string) error {
	// Download script from S3 or HTTP
	script, err := a.downloadScript(scriptURL)
	if err != nil {
		return fmt.Errorf("failed to download checkpoint script: %w", err)
	}

	// Write to temp file
	tmpFile := "/tmp/spawn-checkpoint.sh"
	os.WriteFile(tmpFile, script, 0755)

	// Execute with timeout (leave 30s buffer for cleanup)
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", tmpFile)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("INTERRUPTION_TIME=%s", time.Now().Add(2*time.Minute).Format(time.RFC3339)),
		"INTERRUPTION_ACTION="+a.getTag("spawn:on-interruption"),
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		a.logger.Error("Checkpoint script failed", "output", string(output), "error", err)
		return err
	}

	a.logger.Info("Checkpoint completed", "output", string(output))
	return nil
}

func (a *Agent) sendInterruptionNotification(action *InterruptionAction) {
	// Send SNS notification if configured
	// Log to CloudWatch
	// Update instance tags
}
```

### 5. List Command Changes

**File:** `cmd/list.go`

**Show Spot Indicator:**
```go
func outputTable(instances []aws.InstanceInfo) error {
	// Add SPOT column or indicator in status
	for _, instance := range instances {
		status := instance.State
		if instance.Tags["spawn:spot"] == "true" {
			status += " 💰" // Spot indicator
		}
		// ...
	}
}
```

### 6. Cost Estimation

**File:** `pkg/aws/pricing.go` (new)

```go
// GetSpotPrice returns current spot price for instance type
func (c *Client) GetSpotPrice(ctx context.Context, region, instanceType string) (float64, error) {
	ec2Client := ec2.NewFromConfig(c.cfg)

	// Get spot price history (most recent)
	result, err := ec2Client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)},
		ProductDescriptions: []string{"Linux/UNIX"},
		MaxResults: aws.Int32(1),
		StartTime: aws.Time(time.Now().Add(-1 * time.Hour)),
	})

	if err != nil || len(result.SpotPriceHistory) == 0 {
		return 0, fmt.Errorf("failed to get spot price: %w", err)
	}

	price, _ := strconv.ParseFloat(*result.SpotPriceHistory[0].SpotPrice, 64)
	return price, nil
}

// ShowSpotSavings displays potential savings
func ShowSpotSavings(onDemandPrice, spotPrice float64) {
	savings := ((onDemandPrice - spotPrice) / onDemandPrice) * 100
	fmt.Printf("Spot savings: %.0f%% ($%.4f/hr vs $%.4f/hr on-demand)\n",
		savings, spotPrice, onDemandPrice)
}
```

## Testing Strategy

### Unit Tests

```go
func TestSpotLaunch(t *testing.T) {
	tests := []struct {
		name           string
		config         LaunchConfig
		spotAvailable  bool
		wantSpot       bool
		wantFallback   bool
	}{
		{
			name:          "spot available",
			config:        LaunchConfig{Spot: true},
			spotAvailable: true,
			wantSpot:      true,
		},
		{
			name:          "spot unavailable, no fallback",
			config:        LaunchConfig{Spot: true},
			spotAvailable: false,
			wantSpot:      false,
		},
		{
			name:          "spot unavailable, with fallback",
			config:        LaunchConfig{Spot: true, SpotFallback: true},
			spotAvailable: false,
			wantFallback:  true,
		},
	}
}

func TestInterruptionDetection(t *testing.T) {
	// Mock metadata endpoint
	// Verify detection within 5 seconds
	// Verify handler execution
}

func TestCheckpointExecution(t *testing.T) {
	// Create test checkpoint script
	// Trigger interruption
	// Verify script executed with correct env vars
	// Verify completion within timeout
}
```

### Integration Tests

```bash
# 1. Launch spot instance
spawn launch --instance-type t3.micro --spot --name test-spot

# 2. Verify it's spot
aws ec2 describe-instances --instance-ids i-xxx \
  --query 'Reservations[0].Instances[0].InstanceLifecycle'
# Should return "spot"

# 3. Launch with fallback (simulate unavailable)
spawn launch --instance-type p5.48xlarge --spot --spot-fallback
# Should fall back to on-demand

# 4. Test interruption handling
# (Requires manual AWS console spot interruption simulation)

# 5. Test job array with spot
spawn launch --count 4 --job-array-name test --spot
```

### Edge Cases

1. **Spot capacity unavailable** - Test fallback behavior
2. **Interruption during launch** - Handle gracefully
3. **Checkpoint script timeout** - Ensure cleanup happens
4. **Checkpoint script failure** - Continue with termination
5. **Mixed spot/on-demand job arrays** - Verify correct allocation
6. **Multiple instance types** - Test fleet selection
7. **Max price exceeded** - Proper error message

## User-Data Additions

```bash
# Add to user-data script
export SPAWN_SPOT="true"
export SPAWN_ON_INTERRUPTION="${on_interruption}"
export SPAWN_CHECKPOINT_SCRIPT="${checkpoint_script}"

# Write to config for spored
cat > /etc/spawn/spot-config.json <<EOF
{
  "spot": true,
  "on_interruption": "${on_interruption}",
  "checkpoint_script": "${checkpoint_script}"
}
EOF
```

## Documentation

### User Guide Section

```markdown
## Spot Instances

### Cost Savings

Spot instances can save 70-90% compared to on-demand pricing:

| Instance Type | On-Demand | Spot (avg) | Savings |
|--------------|-----------|------------|---------|
| m7i.large    | $0.1008/hr | $0.0302/hr | 70% |
| g5.xlarge    | $1.006/hr  | $0.302/hr  | 70% |
| r7i.16xlarge | $4.032/hr  | $1.210/hr  | 70% |

### When to Use Spot

✅ **Good for:**
- Batch jobs
- ML training with checkpoints
- CI/CD workers
- Data processing
- Dev/test environments

❌ **Avoid for:**
- Production APIs
- Databases
- Stateful apps without checkpointing

### Basic Usage

```bash
# Launch spot instance
spawn launch --instance-type m7i.large --spot

# With fallback to on-demand
spawn launch --instance-type m7i.large --spot --spot-fallback
```

### Interruption Handling

Spot instances can be interrupted with 2-minute warning. Handle gracefully:

```bash
# Checkpoint before termination
spawn launch --spot --on-interruption checkpoint \
  --checkpoint-script s3://my-bucket/save_checkpoint.sh
```

**Checkpoint Script Example:**
```bash
#!/bin/bash
# save_checkpoint.sh

echo "Saving checkpoint at $(date)"
echo "Interruption in: $INTERRUPTION_TIME"

# Save model checkpoint
python save_checkpoint.py --output /data/checkpoints/

# Upload to S3
aws s3 sync /data/checkpoints/ s3://my-bucket/checkpoints/

echo "Checkpoint saved"
```

### Job Arrays with Spot

```bash
# All spot
spawn launch --count 8 --job-array-name training --spot

# Mixed: 6 spot, 2 on-demand
spawn launch --count 8 --job-array-name training \
  --spot --spot-count 6
```

### Best Practices

1. **Use checkpointing** - Save progress regularly
2. **Multiple instance types** - Better availability
3. **Test interruption handling** - Before production use
4. **Monitor savings** - Track actual costs
5. **Fallback strategy** - Use --spot-fallback for critical jobs
```

## Verification Checklist

- [ ] Can launch single spot instance
- [ ] Can launch with max price
- [ ] Fallback to on-demand works
- [ ] Multiple instance types work
- [ ] Interruption detection works (within 5s)
- [ ] Checkpoint script executes
- [ ] Checkpoint has 90s timeout
- [ ] Graceful shutdown on interruption
- [ ] Job arrays with spot work
- [ ] Mixed spot/on-demand arrays work
- [ ] Spot indicator in `spawn list`
- [ ] Cost estimation accurate
- [ ] Tags correctly applied
- [ ] Works across regions
- [ ] Error messages clear

## Cost Analysis

**Example: 8-instance GPU job array for 4 hours**

On-Demand:
- 8 × g5.xlarge × $1.006/hr × 4hr = $32.19

Spot:
- 8 × g5.xlarge × $0.302/hr × 4hr = $9.66

**Savings: $22.53 (70%)**

For teams running 100+ GPU hours/week, spot can save thousands per month.

## Future Enhancements

1. **Spot block** - Reserve spot for 1-6 hours (no interruption)
2. **Capacity rebalancing** - Proactively move to new instance before interruption
3. **Spot pools** - Automatically maintain N available spot instances
4. **Interruption history** - Track interruption frequency by instance type/AZ
5. **Price alerts** - Notify when spot prices spike
6. **Auto-retry** - Relaunch interrupted instances with same config

## Summary

Spot instance support enables massive cost savings (70-90%) for fault-tolerant workloads. The implementation provides:

- Simple `--spot` flag for easy adoption
- Interruption monitoring and handling via spored
- Checkpoint script execution for graceful shutdown
- Fallback to on-demand for reliability
- Job array support with mixed spot/on-demand
- Multiple instance types for better availability

This feature makes spawn cost-competitive with any cloud management tool while maintaining its simple, opinionated interface.
