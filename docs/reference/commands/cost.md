# spawn cost

Track costs and spending for spawn-managed resources.

## Synopsis

```bash
spawn cost [flags]
spawn cost --instance-id <instance-id> [flags]
spawn cost --sweep-id <sweep-id> [flags]
spawn cost --breakdown [flags]
```

## Description

Track AWS costs for spawn-managed instances, parameter sweeps, and overall spending. Provides real-time cost estimates, historical spending, and cost breakdowns by resource type.

**Features:**
- Real-time cost estimates for running instances
- Historical cost tracking
- Per-instance, per-sweep, or account-wide costs
- Breakdown by instance type, region, and resource type
- Budget tracking and alerts
- Export to CSV/JSON for analysis

## Usage Modes

### Account-Wide Costs
Show total costs for all spawn-managed resources.

```bash
spawn cost
spawn cost --time-range 7d
```

### Instance Costs
Show costs for a specific instance.

```bash
spawn cost --instance-id i-0123456789abcdef0
```

### Sweep Costs
Show costs for a parameter sweep.

```bash
spawn cost --sweep-id sweep-20260127-abc123
```

### Cost Breakdown
Detailed breakdown by resource, region, and instance type.

```bash
spawn cost --breakdown
```

## Flags

### Filtering

#### --instance-id
**Type:** String
**Default:** None
**Description:** Show costs for specific instance.

```bash
spawn cost --instance-id i-0123456789abcdef0
```

#### --sweep-id
**Type:** String
**Default:** None
**Description:** Show costs for parameter sweep.

```bash
spawn cost --sweep-id sweep-20260127-abc123
```

#### --region
**Type:** String
**Default:** All regions
**Description:** Filter by AWS region.

```bash
spawn cost --region us-east-1
```

#### --instance-type
**Type:** String
**Default:** All types
**Description:** Filter by instance type.

```bash
spawn cost --instance-type m7i.large
```

### Time Range

#### --time-range
**Type:** Duration or date range
**Default:** Current month
**Description:** Time period for cost calculation.

```bash
# Last 7 days
spawn cost --time-range 7d

# Last 30 days
spawn cost --time-range 30d

# This month
spawn cost --time-range month

# Last month
spawn cost --time-range last-month

# Specific date range
spawn cost --start-date 2026-01-01 --end-date 2026-01-31
```

#### --start-date
**Type:** String (YYYY-MM-DD)
**Default:** Start of current month
**Description:** Start date for cost calculation.

```bash
spawn cost --start-date 2026-01-01 --end-date 2026-01-31
```

#### --end-date
**Type:** String (YYYY-MM-DD)
**Default:** Today
**Description:** End date for cost calculation.

```bash
spawn cost --start-date 2026-01-01 --end-date 2026-01-31
```

### Output Options

#### --breakdown
**Type:** Boolean
**Default:** `false`
**Description:** Show detailed cost breakdown.

```bash
spawn cost --breakdown
```

#### --group-by
**Type:** String
**Allowed Values:** `region`, `instance-type`, `sweep`, `day`, `week`, `month`
**Default:** None
**Description:** Group costs by dimension.

```bash
spawn cost --group-by region
spawn cost --group-by instance-type
spawn cost --group-by day
```

#### --format
**Type:** String
**Allowed Values:** `table`, `json`, `csv`
**Default:** `table`
**Description:** Output format.

```bash
spawn cost --format json
spawn cost --format csv > costs.csv
```

#### --show-forecast
**Type:** Boolean
**Default:** `false`
**Description:** Include cost forecast for remainder of month.

```bash
spawn cost --show-forecast
```

## Output

### Account-Wide Costs (Default)

```
Spawn Costs - January 2026

Current Month (Jan 1 - Jan 27):
  Total: $456.78
  Daily Average: $16.92
  Projected Month-End: $526.42

By Resource Type:
  EC2 Instances:        $398.45  (87.2%)
  EBS Volumes:          $42.18   (9.2%)
  Data Transfer:        $12.87   (2.8%)
  Lambda:               $2.15    (0.5%)
  DynamoDB:             $1.13    (0.2%)

By Instance Type:
  m7i.large:            $245.67  (53.8%)
  g5.xlarge:            $98.34   (21.5%)
  t3.micro:             $54.44   (11.9%)
  Other:                $58.33   (12.8%)

By Region:
  us-east-1:            $289.12  (63.3%)
  us-west-2:            $123.45  (27.0%)
  ap-south-1:           $44.21   (9.7%)

Top 5 Instances:
  i-0123456789abc  m7i.large    $89.45   20d 5h    us-east-1
  i-0987654321fed  g5.xlarge    $67.89   15d 3h    us-west-2
  i-abcdef012345f  m7i.large    $56.12   12d 8h    us-east-1
  i-543210fedcba9  t3.micro     $34.56   25d 2h    us-east-1
  i-135792468ace0  g5.xlarge    $30.45   8d 12h    us-west-2

Budget Status:
  Monthly Budget:       $500.00
  Current Spend:        $456.78  (91.4%)
  Remaining:            $43.22   (8.6%)
  ⚠ Alert Threshold:    $400.00  (exceeded)
```

### Instance Costs

```
Instance Cost Report

Instance: i-0123456789abcdef0
Name: ml-training-job
Type: g5.xlarge
Region: us-east-1

Lifecycle:
  Launched: 2026-01-20 10:00:00 PST
  Uptime: 7d 3h 15m
  State: running

Costs:
  EC2 Compute:          $45.67  ($0.268/hour × 170.25 hours)
  EBS Storage:          $3.89   (100 GB × 28 days × $0.00139/GB/day)
  Data Transfer:        $1.23   (45 GB × $0.09/GB out)
  Total:                $50.79

Estimated Costs:
  Per Hour:             $0.282
  Per Day:              $6.77
  Projected Monthly:    $203.10  (if running full month)

TTL:
  Remaining: 2h 45m
  Additional Cost: ~$0.78
```

### Sweep Costs

```
Parameter Sweep Cost Report

Sweep: sweep-20260127-abc123
Started: 2026-01-27 14:00:00 PST
Status: completed
Duration: 2h 35m

Instances:
  Total: 50
  Completed: 48
  Failed: 2

Cost Breakdown:
  EC2 Compute:          $23.45  (50 instances × avg $0.469)
  EBS Storage:          $1.89   (50 × 20 GB × 2.58 hours)
  Lambda Orchestration: $0.02   (625 invocations)
  S3 Storage:           $0.45   (Results: 12.3 GB)
  Data Transfer:        $0.89   (28 GB out)
  Total:                $26.70

Per Instance:
  Average:              $0.53
  Min:                  $0.45   (t3.micro, 2h)
  Max:                  $0.68   (m7i.large, 2.5h)

Cost Efficiency:
  Total Cost:           $26.70
  Per Parameter:        $0.53   (50 parameters)
  Per Successful Run:   $0.56   (48 succeeded)
```

### Cost Breakdown

```
Detailed Cost Breakdown - January 2026

By Region:
+------------+-----------+---------+
| Region     | Cost      | Share   |
+------------+-----------+---------+
| us-east-1  | $289.12   | 63.3%   |
| us-west-2  | $123.45   | 27.0%   |
| ap-south-1 | $44.21    | 9.7%    |
+------------+-----------+---------+

By Instance Type:
+-------------+-----------+---------+-----------+
| Type        | Cost      | Share   | Hours     |
+-------------+-----------+---------+-----------+
| m7i.large   | $245.67   | 53.8%   | 1,234     |
| g5.xlarge   | $98.34    | 21.5%   | 456       |
| t3.micro    | $54.44    | 11.9%   | 2,345     |
| c7i.xlarge  | $34.21    | 7.5%    | 178       |
| Other       | $24.12    | 5.3%    | 567       |
+-------------+-----------+---------+-----------+

By Day (Last 7 Days):
+------------+-----------+------------+
| Date       | Cost      | Instances  |
+------------+-----------+------------+
| Jan 27     | $18.45    | 12         |
| Jan 26     | $22.34    | 15         |
| Jan 25     | $19.12    | 13         |
| Jan 24     | $15.67    | 10         |
| Jan 23     | $17.89    | 11         |
| Jan 22     | $21.45    | 14         |
| Jan 21     | $20.12    | 13         |
+------------+-----------+------------+
```

### JSON Output

```json
{
  "period": {
    "start": "2026-01-01",
    "end": "2026-01-27",
    "days": 27
  },
  "total_cost": 456.78,
  "daily_average": 16.92,
  "projected_month_end": 526.42,
  "by_resource_type": {
    "ec2": 398.45,
    "ebs": 42.18,
    "data_transfer": 12.87,
    "lambda": 2.15,
    "dynamodb": 1.13
  },
  "by_instance_type": {
    "m7i.large": 245.67,
    "g5.xlarge": 98.34,
    "t3.micro": 54.44
  },
  "by_region": {
    "us-east-1": 289.12,
    "us-west-2": 123.45,
    "ap-south-1": 44.21
  },
  "budget": {
    "monthly_budget": 500.00,
    "current_spend": 456.78,
    "remaining": 43.22,
    "percent_used": 91.4,
    "alert_threshold": 400.00,
    "alert_triggered": true
  }
}
```

## Examples

### Current Month Costs
```bash
spawn cost
```

### Last 30 Days
```bash
spawn cost --time-range 30d
```

### Specific Date Range
```bash
spawn cost --start-date 2026-01-01 --end-date 2026-01-31
```

### Instance Cost
```bash
spawn cost --instance-id i-0123456789abcdef0
```

### Sweep Cost
```bash
spawn cost --sweep-id sweep-20260127-abc123
```

### Cost Breakdown by Region
```bash
spawn cost --breakdown --group-by region
```

### Cost Breakdown by Instance Type
```bash
spawn cost --breakdown --group-by instance-type
```

### Daily Costs (Last 30 Days)
```bash
spawn cost --time-range 30d --group-by day
```

### Export to CSV
```bash
spawn cost --format csv > costs.csv
```

### JSON for Analysis
```bash
spawn cost --format json | jq '.by_instance_type'
```

### Cost Forecast
```bash
spawn cost --show-forecast
```

## Cost Tracking in Scripts

### Monitor Monthly Budget
```bash
#!/bin/bash
BUDGET=500

COST=$(spawn cost --format json | jq -r '.total_cost')

if (( $(echo "$COST > $BUDGET" | bc -l) )); then
    echo "⚠️  Budget exceeded: \$$COST / \$$BUDGET"
    # Cancel running sweeps
    spawn list-sweeps --status running --quiet | while read sweep_id; do
        spawn cancel --sweep-id "$sweep_id" --terminate-instances
    done
fi
```

### Daily Cost Report
```bash
#!/bin/bash
# Run daily via cron

TODAY=$(date +%Y-%m-%d)
COST=$(spawn cost --start-date "$TODAY" --end-date "$TODAY" --format json)

TOTAL=$(echo "$COST" | jq -r '.total_cost')
INSTANCES=$(spawn list --state running | wc -l)

echo "Daily Cost Report - $TODAY"
echo "Total: \$$TOTAL"
echo "Running Instances: $INSTANCES"
echo ""
spawn cost --start-date "$TODAY" --end-date "$TODAY" --breakdown
```

## Budget Alerts

Configure budget alerts via spawn alerts:

```bash
# Alert when monthly cost exceeds $400
spawn alerts create global \
  --cost-threshold 400 \
  --slack https://hooks.slack.com/... \
  --name "Budget alert"
```

## Cost Estimation

Before launching instances, estimate costs:

```bash
# Estimate single instance cost
# m7i.large: $0.1008/hour
# 24 hours = $2.42

# Estimate sweep cost
# 50 instances × t3.micro ($0.0104/hour) × 2 hours = $1.04

# Use spawn cost to track actual vs estimated
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Cost report generated successfully |
| 1 | Cost calculation failed (AWS API error, missing pricing data) |
| 2 | Invalid time range (invalid date format, end before start) |

## Data Sources

spawn cost uses multiple data sources:

1. **Running Instances:** Real-time EC2 pricing API
2. **Historical Costs:** AWS Cost Explorer API (updated daily)
3. **EBS Volumes:** Volume size × days × EBS pricing
4. **Data Transfer:** S3/CloudWatch metrics × transfer pricing
5. **Lambda:** CloudWatch metrics × Lambda pricing

**Accuracy:** Within 5% of AWS billing, updated hourly.

## Troubleshooting

### "Missing pricing data"
```bash
# Pricing data may not be available for new regions
# Use AWS Cost Explorer for authoritative costs

# Check AWS Cost Explorer
aws ce get-cost-and-usage \
  --time-period Start=2026-01-01,End=2026-01-31 \
  --granularity MONTHLY \
  --metrics BlendedCost \
  --filter file://filter.json
```

### Costs Don't Match AWS Billing
```bash
# spawn cost is an estimate based on:
# - Instance uptime
# - Published pricing
# - EBS volume size/type
# - Estimated data transfer

# AWS billing includes:
# - Reserved Instance discounts
# - Savings Plans
# - Volume discounts
# - Free tier

# For exact costs, use AWS Cost Explorer
```

## See Also
- [spawn launch](launch.md) - Launch instances with budget
- [spawn alerts](alerts.md) - Configure cost alerts
- [spawn list](list.md) - List running instances
- [spawn status](status.md) - Check instance uptime
