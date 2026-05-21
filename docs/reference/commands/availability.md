# spawn availability

Check instance type availability across regions based on historical launch data.

## Synopsis

```bash
spawn availability --instance-type <type> [--regions <regions>]
```

## Description

The `availability` command displays availability statistics for instance types across AWS regions. It analyzes historical launch success/failure data to identify regions with proven capacity.

This helps you:
- Find regions with available capacity for specific instance types
- Identify alternative regions when launches fail
- Understand capacity patterns across regions
- Make informed decisions about region selection

**How it works:**
- Statistics are passively collected from actual launch attempts
- Success/failure rates tracked per instance type per region
- Data stored in DynamoDB for historical analysis
- No active polling or capacity checking (uses real launch data)

## Options

### Required Flags

**`--instance-type <type>`**
- Instance type to check availability for
- Example: `c7i.xlarge`, `g5.2xlarge`, `m7i.4xlarge`

### Optional Flags

**`--regions <regions>`**
- Comma-separated list of regions to check
- Default: Common regions (us-east-1, us-west-2, eu-west-1, ap-southeast-1)
- Example: `us-east-1,us-west-2,eu-central-1`

## Examples

### Check Availability for Instance Type

**Check c7i.xlarge across default regions:**
```bash
spawn availability --instance-type c7i.xlarge
```

**Output:**
```
Instance Type: c7i.xlarge
Region         Success  Failure  Success Rate  Last Success      Last Failure
us-east-1      245      12       95.3%         2026-01-27 10:45  2026-01-25 14:22
us-west-2      189      8        95.9%         2026-01-27 09:30  2026-01-24 16:11
eu-west-1      156      15       91.2%         2026-01-27 08:15  2026-01-26 11:05
ap-southeast-1 98       5        95.1%         2026-01-26 22:10  2026-01-23 09:45
```

### Check Specific Regions

**Check GPU availability in specific regions:**
```bash
spawn availability \
  --instance-type g5.xlarge \
  --regions us-east-1,us-west-2,ap-northeast-1
```

**Output:**
```
Instance Type: g5.xlarge
Region            Success  Failure  Success Rate  Last Success      Last Failure
us-east-1         42       8        84.0%         2026-01-27 10:30  2026-01-27 09:15
us-west-2         38       12       76.0%         2026-01-26 18:45  2026-01-27 08:00
ap-northeast-1    25       3        89.3%         2026-01-26 14:20  2026-01-24 11:30
```

**Interpretation:**
- `ap-northeast-1` has highest success rate (89.3%) → Try here first
- `us-west-2` has lowest success rate (76.0%) → Use as fallback
- Recent failures in `us-east-1` and `us-west-2` → Capacity may be tight

### Check Rare Instance Types

**Check large instance availability:**
```bash
spawn availability --instance-type m7i.48xlarge
```

**Output:**
```
Instance Type: m7i.48xlarge
Region         Success  Failure  Success Rate  Last Success      Last Failure
us-east-1      8        2        80.0%         2026-01-20 15:30  2026-01-18 10:15
us-west-2      5        0        100.0%        2026-01-22 09:45  Never
eu-west-1      3        1        75.0%         2026-01-19 14:20  2026-01-17 11:00
ap-southeast-1 0        0        N/A           Never             Never

Note: Limited data available for this instance type
```

## Use Cases

### 1. Retry Failed Launches in Different Region

**Scenario:** Launch failed in us-east-1 due to insufficient capacity.

```bash
# Check availability
spawn availability --instance-type g5.2xlarge

# Launch in region with best availability
spawn launch --instance-type g5.2xlarge --region us-west-2
```

### 2. Multi-Region Deployment Strategy

**Scenario:** Launching 100 instances, want to distribute across regions.

```bash
# Check availability
spawn availability --instance-type c7i.xlarge

# Launch in best regions based on availability
spawn launch --instance-type c7i.xlarge --array 50 --region us-east-1
spawn launch --instance-type c7i.xlarge --array 50 --region us-west-2
```

### 3. Spot Instance Region Selection

**Scenario:** Using spot instances, want to maximize launch success rate.

```bash
# Check availability for spot-friendly instance type
spawn availability --instance-type c7i.2xlarge

# Launch spot in region with highest success rate
spawn launch --instance-type c7i.2xlarge --spot --region <best-region>
```

## Understanding Statistics

### Success Rate Interpretation

| Success Rate | Meaning | Action |
|--------------|---------|--------|
| 95%+ | Excellent availability | Preferred region |
| 85-95% | Good availability | Reliable choice |
| 70-85% | Moderate availability | Use with caution |
| <70% | Poor availability | Avoid or use fallback |

### Temporal Patterns

**Recent failures matter more:**
- Last failure today → Capacity may be tight now
- Last failure weeks ago → Likely temporary issue resolved
- No recent launches → Data may be stale

**Time of day effects:**
- Peak hours (9-5 EST) → Higher contention
- Off-peak hours → Better availability
- Weekend vs weekday patterns

### Instance Type Factors

**Common types (t3, c7i, m7i):**
- Generally high availability (95%+)
- Many availability zones
- Consistent capacity

**GPU types (g5, p4d, p5):**
- Lower availability (70-90%)
- Concentrated in specific zones
- More failures during peak ML training times

**Large instances (*.48xlarge, metal):**
- Variable availability
- Fewer hosts available
- May require quota increases

## Limitations

### 1. Historical Data Only

**What it shows:**
- Past launch success/failure patterns
- Regions that have historically worked well

**What it doesn't show:**
- Real-time capacity
- Current availability
- Future capacity

### 2. User-Specific Data

**Statistics reflect:**
- Your launch attempts only
- Your account quota limits
- Your typical usage patterns

**Not reflected:**
- Other users' capacity usage
- AWS-wide capacity constraints
- Spot instance pool depth

### 3. Sample Size Matters

**Reliable data requires:**
- 10+ launches per region minimum
- Recent launches (within last 30 days)
- Representative workload mix

**Low sample size:**
- Statistics may not be meaningful
- Use with caution for rare instance types

## Integration with Launch

### Automatic Fallback (Future Feature)

**Planned functionality:**
```bash
# Auto-select region based on availability
spawn launch --instance-type g5.xlarge --best-region

# Auto-fallback on failure
spawn launch --instance-type c7i.xlarge --fallback-regions us-east-1,us-west-2,eu-west-1
```

### Manual Region Selection

**Current workflow:**
```bash
# 1. Check availability
BEST_REGION=$(spawn availability --instance-type c7i.xlarge | \
  grep -v "Instance Type" | tail -n +2 | \
  sort -k4 -rn | head -1 | awk '{print $1}')

# 2. Launch in best region
spawn launch --instance-type c7i.xlarge --region $BEST_REGION
```

## Data Collection

### How Data is Collected

**Automatic tracking:**
- Every `spawn launch` records success/failure
- Region, instance type, and timestamp stored
- No user action required

**Data stored in DynamoDB:**
- Table: `spawn-availability-stats`
- Partition key: `instance_type`
- Sort key: `region#timestamp`

### Privacy Considerations

**What is tracked:**
- Instance type
- Region
- Success/failure status
- Timestamp

**What is NOT tracked:**
- User identity
- Instance content
- Application data
- Sensitive information

### Data Retention

**Retention policy:**
- 90 days of historical data
- Older data automatically purged
- Recent data weighted more heavily

## Troubleshooting

### No Data Available

**Problem:** "No availability data for instance type"

**Causes:**
1. Never launched this instance type before
2. All launches older than 90 days
3. DynamoDB table not initialized

**Solution:**
```bash
# Launch instance to generate data
spawn launch --instance-type c7i.xlarge --ttl 5m

# Check again after launch
spawn availability --instance-type c7i.xlarge
```

### Unexpected Low Success Rate

**Problem:** Success rate lower than expected

**Possible reasons:**
1. Account quota limit reached
2. Invalid instance type for region
3. Network issues during launch
4. Temporary capacity constraints

**Debugging:**
```bash
# Check quota limits
aws service-quotas get-service-quota \
  --service-code ec2 \
  --quota-code L-1216C47A

# Verify instance type supported in region
aws ec2 describe-instance-types \
  --instance-types c7i.xlarge \
  --region us-east-1
```

### Stale Data

**Problem:** Last launch data is old

**Solution:**
- Launch new instances to refresh data
- Data automatically updates on each launch
- No manual refresh needed

## Related Commands

- **[spawn launch](launch.md)** - Launch instances (generates availability data)
- **[spawn list](list.md)** - List running instances
- **[spawn status](status.md)** - Check instance status

## See Also

- [How-To: Launch Instances](../../how-to/launch-instances.md) - Launch patterns
- [How-To: Cost Optimization](../../how-to/cost-optimization.md) - Regional cost differences
- [Tutorial 1: Getting Started](../../tutorials/01-getting-started.md) - Basic usage
- [AWS Instance Types](https://aws.amazon.com/ec2/instance-types/) - Instance type reference
