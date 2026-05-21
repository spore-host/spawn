# How-To: Monitoring at Scale

Monitor and observe large spawn deployments (hundreds or thousands of instances).

## Overview

### Monitoring Layers

1. **Instance-level** - Individual instance health, resource usage
2. **Array-level** - Job array progress, completion rates
3. **Fleet-level** - Total capacity, cost, efficiency
4. **Application-level** - Custom metrics from your workload

---

## CloudWatch Dashboards

### Problem
Managing 500+ instances without visibility into fleet health.

### Solution: Unified CloudWatch Dashboard

**Create dashboard:**
```bash
#!/bin/bash
# create-spawn-dashboard.sh

REGION="us-east-1"

cat > dashboard.json << 'EOF'
{
  "widgets": [
    {
      "type": "metric",
      "properties": {
        "title": "Active Instances",
        "metrics": [
          [ "AWS/EC2", "CPUUtilization", { "stat": "SampleCount", "period": 300 } ]
        ],
        "region": "us-east-1",
        "yAxis": { "left": { "label": "Count" } }
      }
    },
    {
      "type": "metric",
      "properties": {
        "title": "Fleet CPU Utilization",
        "metrics": [
          [ "AWS/EC2", "CPUUtilization", { "stat": "Average" } ],
          [ "...", { "stat": "Maximum" } ],
          [ "...", { "stat": "Minimum" } ]
        ],
        "region": "us-east-1",
        "yAxis": { "left": { "label": "Percent" } }
      }
    },
    {
      "type": "metric",
      "properties": {
        "title": "Network Throughput",
        "metrics": [
          [ "AWS/EC2", "NetworkIn", { "stat": "Sum" } ],
          [ ".", "NetworkOut", { "stat": "Sum" } ]
        ],
        "region": "us-east-1",
        "yAxis": { "left": { "label": "Bytes" } }
      }
    },
    {
      "type": "metric",
      "properties": {
        "title": "Spot Interruptions",
        "metrics": [
          [ "AWS/EC2", "StatusCheckFailed", { "stat": "Sum" } ]
        ],
        "region": "us-east-1",
        "period": 300
      }
    }
  ]
}
EOF

aws cloudwatch put-dashboard \
  --dashboard-name spawn-fleet \
  --dashboard-body file://dashboard.json \
  --region $REGION

echo "Dashboard created: https://console.aws.amazon.com/cloudwatch/home?region=${REGION}#dashboards:name=spawn-fleet"
```

**Filter by spawn tag:**
```json
{
  "metrics": [
    [ {
      "expression": "SELECT AVG(CPUUtilization) FROM SCHEMA(\"AWS/EC2\", InstanceId) WHERE spawn='true' GROUP BY spawn",
      "id": "q1"
    } ]
  ]
}
```

---

## Custom Metrics from spored

### Problem
Need application-specific metrics (tasks completed, GPU utilization, custom events).

### Solution: CloudWatch Custom Metrics

**Report from spored agent:**
```bash
#!/bin/bash
# worker-with-metrics.sh

INSTANCE_ID=$(ec2-metadata --instance-id | cut -d' ' -f2)
NAMESPACE="Spawn/Workers"

report_metric() {
  local metric_name=$1
  local value=$2
  local unit=${3:-None}

  aws cloudwatch put-metric-data \
    --namespace "$NAMESPACE" \
    --metric-name "$metric_name" \
    --value "$value" \
    --unit "$unit" \
    --dimensions InstanceId="$INSTANCE_ID",ArrayIndex="$TASK_ARRAY_INDEX"
}

# Process tasks
COMPLETED=0
ERRORS=0
START_TIME=$(date +%s)

while process_task; do
  COMPLETED=$((COMPLETED + 1))

  # Report progress every 10 tasks
  if [ $((COMPLETED % 10)) -eq 0 ]; then
    report_metric "TasksCompleted" $COMPLETED "Count"
    report_metric "ErrorCount" $ERRORS "Count"

    # Report task duration
    CURRENT_TIME=$(date +%s)
    ELAPSED=$((CURRENT_TIME - START_TIME))
    RATE=$(awk "BEGIN {print $COMPLETED / $ELAPSED}")
    report_metric "TaskRate" "$RATE" "Count/Second"
  fi
done

# Final metrics
report_metric "TasksCompleted" $COMPLETED "Count"
report_metric "ErrorCount" $ERRORS "Count"

echo "Completed $COMPLETED tasks with $ERRORS errors"
spored complete --status success
```

**GPU metrics:**
```bash
#!/bin/bash
# report-gpu-metrics.sh

while true; do
  # Get GPU utilization from nvidia-smi
  GPU_UTIL=$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits | head -1)
  GPU_MEM=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits | head -1)
  GPU_TEMP=$(nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader,nounits | head -1)

  aws cloudwatch put-metric-data \
    --namespace "Spawn/GPU" \
    --metric-name "GPUUtilization" \
    --value "$GPU_UTIL" \
    --unit "Percent" \
    --dimensions InstanceId="$INSTANCE_ID"

  aws cloudwatch put-metric-data \
    --namespace "Spawn/GPU" \
    --metric-name "GPUMemoryUsed" \
    --value "$GPU_MEM" \
    --unit "Megabytes" \
    --dimensions InstanceId="$INSTANCE_ID"

  aws cloudwatch put-metric-data \
    --namespace "Spawn/GPU" \
    --metric-name "GPUTemperature" \
    --value "$GPU_TEMP" \
    --unit "None" \
    --dimensions InstanceId="$INSTANCE_ID"

  sleep 60
done
```

**Launch with metrics:**
```bash
spawn launch \
  --instance-type g5.xlarge \
  --array 100 \
  --iam-policy cloudwatch:WriteOnly \
  --user-data @worker-with-metrics.sh
```

---

## Log Aggregation

### Problem
Viewing logs from 1000 instances individually is impractical.

### Solution: CloudWatch Logs with Log Groups

**Setup CloudWatch agent:**
```bash
#!/bin/bash
# setup-cloudwatch-logs.sh

# Install CloudWatch agent
sudo yum install -y amazon-cloudwatch-agent

# Configure agent
cat > /opt/aws/amazon-cloudwatch-agent/etc/config.json << EOF
{
  "logs": {
    "logs_collected": {
      "files": {
        "collect_list": [
          {
            "file_path": "/var/log/spored.log",
            "log_group_name": "/aws/spawn/spored",
            "log_stream_name": "{instance_id}",
            "timestamp_format": "%Y-%m-%d %H:%M:%S"
          },
          {
            "file_path": "/home/ec2-user/worker.log",
            "log_group_name": "/aws/spawn/workers",
            "log_stream_name": "array-${TASK_ARRAY_INDEX}-{instance_id}",
            "timestamp_format": "%Y-%m-%d %H:%M:%S"
          }
        ]
      }
    }
  }
}
EOF

# Start agent
sudo /opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl \
  -a fetch-config \
  -m ec2 \
  -s \
  -c file:/opt/aws/amazon-cloudwatch-agent/etc/config.json
```

**Query logs across all instances:**
```bash
# Get logs from all workers in the last hour
aws logs filter-log-events \
  --log-group-name /aws/spawn/workers \
  --start-time $(($(date +%s) - 3600))000 \
  --filter-pattern "ERROR"

# Count errors by instance
aws logs filter-log-events \
  --log-group-name /aws/spawn/workers \
  --start-time $(($(date +%s) - 3600))000 \
  --filter-pattern "ERROR" \
  | jq -r '.events[].logStreamName' \
  | sort | uniq -c | sort -rn
```

**CloudWatch Insights queries:**
```sql
-- Top 10 slowest tasks
fields @timestamp, task_id, duration
| filter @message like /Task completed/
| parse @message "Task * completed in *s" as task_id, duration
| sort duration desc
| limit 10

-- Error rate by hour
fields @timestamp, @message
| filter @message like /ERROR/
| stats count() as errors by bin(1h)

-- Tasks completed per worker
fields @logStream, @message
| filter @message like /Task completed/
| stats count() as tasks by @logStream
| sort tasks desc
```

---

## Array Progress Monitoring

### Problem
Track progress of 1000-instance job array in real time.

### Solution: DynamoDB Progress Table + Monitoring Script

**Worker reports progress:**
```bash
#!/bin/bash
# worker-tracked.sh

ARRAY_ID="array-20260127-abc123"
PROGRESS_TABLE="spawn-progress"

update_progress() {
  aws dynamodb put-item \
    --table-name $PROGRESS_TABLE \
    --item "{
      \"array_id\": {\"S\": \"$ARRAY_ID\"},
      \"task_index\": {\"N\": \"$TASK_ARRAY_INDEX\"},
      \"status\": {\"S\": \"$1\"},
      \"tasks_completed\": {\"N\": \"$2\"},
      \"last_update\": {\"S\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"},
      \"instance_id\": {\"S\": \"$INSTANCE_ID\"},
      \"hostname\": {\"S\": \"$(hostname)\"}
    }"
}

update_progress "running" 0

COMPLETED=0
while process_task; do
  COMPLETED=$((COMPLETED + 1))

  if [ $((COMPLETED % 10)) -eq 0 ]; then
    update_progress "running" $COMPLETED
  fi
done

update_progress "completed" $COMPLETED
```

**Real-time monitoring dashboard:**
```bash
#!/bin/bash
# monitor-array.sh

ARRAY_ID="array-20260127-abc123"
PROGRESS_TABLE="spawn-progress"

while true; do
  # Query progress table
  ITEMS=$(aws dynamodb scan \
    --table-name $PROGRESS_TABLE \
    --filter-expression "array_id = :array_id" \
    --expression-attribute-values '{":array_id":{"S":"'$ARRAY_ID'"}}' \
    --query 'Items[*].[task_index.N, status.S, tasks_completed.N, last_update.S]' \
    --output text)

  # Calculate statistics
  TOTAL_WORKERS=$(echo "$ITEMS" | wc -l)
  RUNNING=$(echo "$ITEMS" | grep -c "running")
  COMPLETED=$(echo "$ITEMS" | grep -c "completed")
  FAILED=$(echo "$ITEMS" | grep -c "failed")
  TOTAL_TASKS=$(echo "$ITEMS" | awk '{sum += $3} END {print sum}')

  # Calculate ETA
  ELAPSED=$(($(date +%s) - $(date -d "2026-01-27 10:00:00" +%s)))
  if [ $COMPLETED -gt 0 ]; then
    AVG_TIME=$((ELAPSED / COMPLETED))
    REMAINING=$((TOTAL_WORKERS - COMPLETED))
    ETA_SECONDS=$((REMAINING * AVG_TIME))
    ETA=$(date -d "@$(($(date +%s) + ETA_SECONDS))" "+%H:%M:%S")
  else
    ETA="Calculating..."
  fi

  clear
  echo "========================================="
  echo "Array Progress: $ARRAY_ID"
  echo "========================================="
  echo ""
  echo "Workers:"
  echo "  Total:     $TOTAL_WORKERS"
  echo "  Running:   $RUNNING"
  echo "  Completed: $COMPLETED"
  echo "  Failed:    $FAILED"
  echo ""
  echo "Tasks Processed: $TOTAL_TASKS"
  echo ""
  echo "Progress: $((COMPLETED * 100 / TOTAL_WORKERS))%"
  echo "ETA: $ETA"
  echo ""
  echo "Recent updates:"
  echo "$ITEMS" | sort -k4 -r | head -5 | column -t

  sleep 10
done
```

---

## Fleet Health Monitoring

### Problem
Detect unhealthy instances in fleet of 500+ instances.

### Solution: EC2 Status Checks + Alarms

**Create status check alarm for fleet:**
```bash
#!/bin/bash
# create-fleet-alarms.sh

# Get all spawn instances
INSTANCES=$(aws ec2 describe-instances \
  --filters "Name=tag:spawn,Values=true" "Name=instance-state-name,Values=running" \
  --query 'Reservations[*].Instances[*].InstanceId' \
  --output text)

for INSTANCE_ID in $INSTANCES; do
  # Instance status check alarm
  aws cloudwatch put-metric-alarm \
    --alarm-name "spawn-instance-${INSTANCE_ID}-status" \
    --alarm-description "Instance status check failed" \
    --metric-name StatusCheckFailed_Instance \
    --namespace AWS/EC2 \
    --statistic Maximum \
    --period 60 \
    --evaluation-periods 2 \
    --threshold 1 \
    --comparison-operator GreaterThanOrEqualToThreshold \
    --dimensions Name=InstanceId,Value=$INSTANCE_ID \
    --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-alerts

  # System status check alarm
  aws cloudwatch put-metric-alarm \
    --alarm-name "spawn-instance-${INSTANCE_ID}-system" \
    --alarm-description "System status check failed" \
    --metric-name StatusCheckFailed_System \
    --namespace AWS/EC2 \
    --statistic Maximum \
    --period 60 \
    --evaluation-periods 2 \
    --threshold 1 \
    --comparison-operator GreaterThanOrEqualToThreshold \
    --dimensions Name=InstanceId,Value=$INSTANCE_ID \
    --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-alerts
done

echo "Created alarms for $(echo $INSTANCES | wc -w) instances"
```

**Composite alarm for fleet health:**
```bash
# Alert if >5% of fleet is unhealthy
aws cloudwatch put-composite-alarm \
  --alarm-name spawn-fleet-health \
  --alarm-description "More than 5% of fleet unhealthy" \
  --alarm-rule "ALARM(spawn-instance-*-status) OR ALARM(spawn-instance-*-system)" \
  --actions-enabled \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-critical
```

---

## Cost Monitoring

### Problem
Track spending across large spawn deployments.

### Solution: Cost Allocation Tags + CloudWatch Billing Metrics

**Tag instances for cost tracking:**
```bash
spawn launch \
  --instance-type g5.xlarge \
  --array 100 \
  --tags project=ml-training,cost-center=research,owner=alice
```

**Create cost dashboard:**
```bash
#!/bin/bash
# create-cost-dashboard.sh

cat > cost-dashboard.json << 'EOF'
{
  "widgets": [
    {
      "type": "metric",
      "properties": {
        "title": "Estimated Daily Cost",
        "metrics": [
          [ "AWS/Billing", "EstimatedCharges", { "stat": "Maximum" } ]
        ],
        "region": "us-east-1",
        "period": 86400
      }
    },
    {
      "type": "metric",
      "properties": {
        "title": "EC2 Cost by Instance Type",
        "metrics": [
          [ {
            "expression": "SELECT SUM(EstimatedCharges) FROM SCHEMA(\"AWS/Billing\", ServiceName, InstanceType) WHERE ServiceName='AmazonEC2' GROUP BY InstanceType"
          } ]
        ],
        "region": "us-east-1"
      }
    }
  ]
}
EOF

aws cloudwatch put-dashboard \
  --dashboard-name spawn-costs \
  --dashboard-body file://cost-dashboard.json
```

**Cost alert:**
```bash
# Alert if daily cost exceeds $1000
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-daily-cost-limit \
  --alarm-description "Daily spawn cost exceeds budget" \
  --metric-name EstimatedCharges \
  --namespace AWS/Billing \
  --statistic Maximum \
  --period 86400 \
  --evaluation-periods 1 \
  --threshold 1000 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-billing-alerts
```

**Query cost by tag:**
```bash
# Requires Cost Explorer API
aws ce get-cost-and-usage \
  --time-period Start=2026-01-01,End=2026-01-31 \
  --granularity DAILY \
  --metrics BlendedCost \
  --group-by Type=TAG,Key=project \
  --filter '{"Tags":{"Key":"spawn","Values":["true"]}}'
```

---

## Anomaly Detection

### Problem
Detect unusual patterns (cost spikes, error rate increases, performance degradation).

### Solution: CloudWatch Anomaly Detection

**Create anomaly detector for CPU:**
```bash
# CloudWatch learns normal CPU patterns
aws cloudwatch put-anomaly-detector \
  --namespace AWS/EC2 \
  --metric-name CPUUtilization \
  --stat Average \
  --dimensions Name=spawn,Value=true

# Create alarm based on anomaly detection
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-cpu-anomaly \
  --alarm-description "Unusual CPU usage pattern detected" \
  --metric-name CPUUtilization \
  --namespace AWS/EC2 \
  --statistic Average \
  --period 300 \
  --evaluation-periods 2 \
  --threshold-metric-id e1 \
  --comparison-operator LessThanLowerOrGreaterThanUpperThreshold \
  --metrics '[
    {
      "Id": "m1",
      "ReturnData": true,
      "MetricStat": {
        "Metric": {
          "Namespace": "AWS/EC2",
          "MetricName": "CPUUtilization",
          "Dimensions": [{"Name": "spawn", "Value": "true"}]
        },
        "Period": 300,
        "Stat": "Average"
      }
    },
    {
      "Id": "e1",
      "Expression": "ANOMALY_DETECTION_BAND(m1, 2)"
    }
  ]' \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-alerts
```

**Cost anomaly detection:**
```bash
# Detect unusual spending patterns
aws cloudwatch put-anomaly-detector \
  --namespace AWS/Billing \
  --metric-name EstimatedCharges \
  --stat Maximum \
  --dimensions Name=ServiceName,Value=AmazonEC2

aws cloudwatch put-metric-alarm \
  --alarm-name spawn-cost-anomaly \
  --alarm-description "Unusual spending pattern detected" \
  --metric-name EstimatedCharges \
  --namespace AWS/Billing \
  --statistic Maximum \
  --period 86400 \
  --evaluation-periods 1 \
  --threshold-metric-id e1 \
  --comparison-operator LessThanLowerOrGreaterThanUpperThreshold \
  --metrics '[
    {
      "Id": "m1",
      "MetricStat": {
        "Metric": {"Namespace": "AWS/Billing", "MetricName": "EstimatedCharges"},
        "Period": 86400,
        "Stat": "Maximum"
      }
    },
    {
      "Id": "e1",
      "Expression": "ANOMALY_DETECTION_BAND(m1, 2)"
    }
  ]'
```

---

## Centralized Monitoring Dashboard

### Problem
Need single pane of glass for entire spawn infrastructure.

### Solution: Comprehensive Grafana/CloudWatch Dashboard

**Create master dashboard:**
```python
#!/usr/bin/env python3
# create-monitoring-dashboard.py

import boto3
import json

cloudwatch = boto3.client('cloudwatch')

dashboard = {
    "widgets": [
        # Row 1: Fleet Overview
        {
            "type": "metric",
            "properties": {
                "title": "Active Instances",
                "metrics": [
                    ["AWS/EC2", "CPUUtilization", {"stat": "SampleCount"}]
                ],
                "period": 300,
                "region": "us-east-1"
            },
            "width": 6,
            "height": 6,
            "x": 0,
            "y": 0
        },
        {
            "type": "metric",
            "properties": {
                "title": "Fleet CPU Utilization",
                "metrics": [
                    ["AWS/EC2", "CPUUtilization", {"stat": "Average"}]
                ],
                "period": 300,
                "yAxis": {"left": {"min": 0, "max": 100}}
            },
            "width": 6,
            "height": 6,
            "x": 6,
            "y": 0
        },
        {
            "type": "metric",
            "properties": {
                "title": "Network I/O",
                "metrics": [
                    ["AWS/EC2", "NetworkIn", {"stat": "Sum", "label": "In"}],
                    [".", "NetworkOut", {"stat": "Sum", "label": "Out"}]
                ],
                "period": 300
            },
            "width": 6,
            "height": 6,
            "x": 12,
            "y": 0
        },
        {
            "type": "metric",
            "properties": {
                "title": "Estimated Daily Cost",
                "metrics": [
                    ["AWS/Billing", "EstimatedCharges", {"stat": "Maximum"}]
                ],
                "period": 86400
            },
            "width": 6,
            "height": 6,
            "x": 18,
            "y": 0
        },

        # Row 2: Application Metrics
        {
            "type": "metric",
            "properties": {
                "title": "Tasks Completed (All Workers)",
                "metrics": [
                    ["Spawn/Workers", "TasksCompleted", {"stat": "Sum"}]
                ],
                "period": 300
            },
            "width": 8,
            "height": 6,
            "x": 0,
            "y": 6
        },
        {
            "type": "metric",
            "properties": {
                "title": "Error Rate",
                "metrics": [
                    ["Spawn/Workers", "ErrorCount", {"stat": "Sum"}]
                ],
                "period": 300
            },
            "width": 8,
            "height": 6,
            "x": 8,
            "y": 6
        },
        {
            "type": "metric",
            "properties": {
                "title": "Task Completion Rate",
                "metrics": [
                    ["Spawn/Workers", "TaskRate", {"stat": "Average"}]
                ],
                "period": 300,
                "yAxis": {"left": {"label": "Tasks/Second"}}
            },
            "width": 8,
            "height": 6,
            "x": 16,
            "y": 6
        },

        # Row 3: GPU Metrics
        {
            "type": "metric",
            "properties": {
                "title": "GPU Utilization",
                "metrics": [
                    ["Spawn/GPU", "GPUUtilization", {"stat": "Average"}]
                ],
                "period": 300,
                "yAxis": {"left": {"min": 0, "max": 100}}
            },
            "width": 8,
            "height": 6,
            "x": 0,
            "y": 12
        },
        {
            "type": "metric",
            "properties": {
                "title": "GPU Memory",
                "metrics": [
                    ["Spawn/GPU", "GPUMemoryUsed", {"stat": "Average"}]
                ],
                "period": 300
            },
            "width": 8,
            "height": 6,
            "x": 8,
            "y": 12
        },
        {
            "type": "metric",
            "properties": {
                "title": "GPU Temperature",
                "metrics": [
                    ["Spawn/GPU", "GPUTemperature", {"stat": "Average"}]
                ],
                "period": 300
            },
            "width": 8,
            "height": 6,
            "x": 16,
            "y": 12
        },

        # Row 4: Health & Alerts
        {
            "type": "metric",
            "properties": {
                "title": "Status Check Failures",
                "metrics": [
                    ["AWS/EC2", "StatusCheckFailed", {"stat": "Sum"}],
                    [".", "StatusCheckFailed_Instance", {"stat": "Sum"}],
                    [".", "StatusCheckFailed_System", {"stat": "Sum"}]
                ],
                "period": 300
            },
            "width": 12,
            "height": 6,
            "x": 0,
            "y": 18
        },
        {
            "type": "metric",
            "properties": {
                "title": "Spot Interruptions",
                "metrics": [
                    ["Spawn/SpotInterruptions", "Count", {"stat": "Sum"}]
                ],
                "period": 300
            },
            "width": 12,
            "height": 6,
            "x": 12,
            "y": 18
        }
    ]
}

response = cloudwatch.put_dashboard(
    DashboardName='spawn-master',
    DashboardBody=json.dumps(dashboard)
)

print("Master dashboard created: spawn-master")
print(f"URL: https://console.aws.amazon.com/cloudwatch/home?region=us-east-1#dashboards:name=spawn-master")
```

---

## Performance Monitoring

### Problem
Identify performance bottlenecks in distributed workload.

### Solution: Distributed Tracing with Custom Metrics

**Worker reports timing metrics:**
```bash
#!/bin/bash
# worker-performance-tracking.sh

report_timing() {
  local operation=$1
  local duration_ms=$2

  aws cloudwatch put-metric-data \
    --namespace "Spawn/Performance" \
    --metric-name "${operation}Duration" \
    --value "$duration_ms" \
    --unit "Milliseconds" \
    --dimensions Operation="$operation",InstanceType="$(ec2-metadata --instance-type | cut -d' ' -f2)"
}

# Track download time
START=$(date +%s%3N)
aws s3 cp s3://bucket/data.tar.gz /tmp/data.tar.gz
END=$(date +%s%3N)
report_timing "S3Download" $((END - START))

# Track processing time
START=$(date +%s%3N)
python process.py /tmp/data.tar.gz /tmp/result.json
END=$(date +%s%3N)
report_timing "Processing" $((END - START))

# Track upload time
START=$(date +%s%3N)
aws s3 cp /tmp/result.json s3://bucket/results/result-${TASK_ARRAY_INDEX}.json
END=$(date +%s%3N)
report_timing "S3Upload" $((END - START))
```

**Analyze performance:**
```bash
# Get P50, P90, P99 processing times
aws cloudwatch get-metric-statistics \
  --namespace "Spawn/Performance" \
  --metric-name "ProcessingDuration" \
  --start-time $(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%S) \
  --end-time $(date -u +%Y-%m-%dT%H:%M:%S) \
  --period 3600 \
  --statistics Average,Minimum,Maximum \
  --extended-statistics p50,p90,p99
```

---

## Alerting at Scale

### Problem
Configure alerts for 500+ instances without creating 500+ individual alarms.

### Solution: Metric Math + Composite Alarms

**Fleet-wide error rate alarm:**
```bash
# Alert if >5% of tasks are failing
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-fleet-error-rate \
  --alarm-description "High error rate across fleet" \
  --evaluation-periods 2 \
  --datapoints-to-alarm 2 \
  --threshold 5 \
  --comparison-operator GreaterThanThreshold \
  --metrics '[
    {
      "Id": "errors",
      "MetricStat": {
        "Metric": {
          "Namespace": "Spawn/Workers",
          "MetricName": "ErrorCount"
        },
        "Period": 300,
        "Stat": "Sum"
      },
      "ReturnData": false
    },
    {
      "Id": "completed",
      "MetricStat": {
        "Metric": {
          "Namespace": "Spawn/Workers",
          "MetricName": "TasksCompleted"
        },
        "Period": 300,
        "Stat": "Sum"
      },
      "ReturnData": false
    },
    {
      "Id": "error_rate",
      "Expression": "(errors / completed) * 100",
      "ReturnData": true
    }
  ]' \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-alerts
```

**Multi-metric composite alarm:**
```bash
# Alert if any of: high error rate, low throughput, or high cost
aws cloudwatch put-composite-alarm \
  --alarm-name spawn-critical-issues \
  --alarm-description "Critical issues detected in spawn fleet" \
  --alarm-rule "(ALARM(spawn-fleet-error-rate) OR ALARM(spawn-low-throughput) OR ALARM(spawn-cost-anomaly))" \
  --actions-enabled \
  --alarm-actions arn:aws:sns:us-east-1:123456789012:spawn-critical \
  --ok-actions arn:aws:sns:us-east-1:123456789012:spawn-critical
```

---

## Log-Based Metrics

### Problem
Extract metrics from application logs without modifying code.

### Solution: CloudWatch Logs Metric Filters

**Create metric filter:**
```bash
# Extract task duration from logs
aws logs put-metric-filter \
  --log-group-name /aws/spawn/workers \
  --filter-name task-duration \
  --filter-pattern '[timestamp, level, msg="Task completed in", duration, unit="seconds"]' \
  --metric-transformations \
    metricName=TaskDuration,\
    metricNamespace=Spawn/LogMetrics,\
    metricValue='$duration',\
    unit=Seconds

# Count errors by type
aws logs put-metric-filter \
  --log-group-name /aws/spawn/workers \
  --filter-name error-types \
  --filter-pattern '[timestamp, level=ERROR, error_type, ...]' \
  --metric-transformations \
    metricName=ErrorsByType,\
    metricNamespace=Spawn/LogMetrics,\
    metricValue=1,\
    unit=Count,\
    defaultValue=0,\
    dimensions='{ErrorType=$error_type}'
```

---

## Monitoring Best Practices

### 1. Consistent Tagging
```bash
# Always tag instances for monitoring
spawn launch --tags \
  project=ml-training,\
  team=research,\
  environment=production,\
  cost-center=ai-lab
```

### 2. Namespace Organization
```
Spawn/Workers       - Application metrics
Spawn/GPU           - GPU metrics
Spawn/Performance   - Timing metrics
Spawn/Health        - Health checks
```

### 3. Retention Policies
```bash
# Set log retention (reduce costs)
aws logs put-retention-policy \
  --log-group-name /aws/spawn/workers \
  --retention-in-days 7

# Keep critical logs longer
aws logs put-retention-policy \
  --log-group-name /aws/spawn/errors \
  --retention-in-days 30
```

### 4. Sampling for High Volume
```bash
# Report metrics every 10 tasks, not every task
if [ $((COMPLETED % 10)) -eq 0 ]; then
  report_metric "TasksCompleted" $COMPLETED
fi
```

---

## Troubleshooting Monitoring Issues

### Missing Metrics

**Problem:** Metrics not appearing in CloudWatch.

**Causes:**
1. IAM permissions missing
2. Incorrect namespace/metric name
3. Agent not running

**Solution:**
```bash
# Check IAM permissions
aws iam get-role-policy \
  --role-name spawn-instance-role \
  --policy-name cloudwatch-policy

# Verify agent running
sudo systemctl status amazon-cloudwatch-agent

# Test metric submission manually
aws cloudwatch put-metric-data \
  --namespace "Test" \
  --metric-name "TestMetric" \
  --value 1
```

### High CloudWatch Costs

**Problem:** CloudWatch costs unexpectedly high.

**Causes:**
1. Too many custom metrics
2. High-frequency metric reporting
3. Long log retention

**Solution:**
```bash
# Reduce metric frequency (1 min → 5 min)
aws cloudwatch put-metric-data --period 300 ...

# Reduce log retention
aws logs put-retention-policy --retention-in-days 3

# Use metric filters instead of custom metrics
# (charged per filter, not per data point)
```

---

## See Also

- [How-To: Cost Optimization](cost-optimization.md) - Reduce monitoring costs
- [How-To: Job Arrays](job-arrays.md) - Progress tracking patterns
- [spawn launch](../reference/commands/launch.md) - Tagging for monitoring
- [AWS CloudWatch Documentation](https://docs.aws.amazon.com/cloudwatch/)
