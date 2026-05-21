# Prometheus Configuration for Spawn

This directory contains Prometheus configuration for monitoring spawn instances.

## Quick Start

### 1. Install Prometheus and Alertmanager

```bash
# macOS
brew install prometheus alertmanager

# Linux (download from https://prometheus.io/download/)
wget https://github.com/prometheus/prometheus/releases/download/v2.45.0/prometheus-2.45.0.linux-amd64.tar.gz
tar xvf prometheus-2.45.0.linux-amd64.tar.gz
cd prometheus-2.45.0.linux-amd64

wget https://github.com/prometheus/alertmanager/releases/download/v0.26.0/alertmanager-0.26.0.linux-amd64.tar.gz
tar xvf alertmanager-0.26.0.linux-amd64.tar.gz
cd alertmanager-0.26.0.linux-amd64
```

### 2. Configure Prometheus

Copy the configuration files to your Prometheus directory:

```bash
# Copy main config
cp prometheus.yaml /etc/prometheus/prometheus.yml

# Copy alert rules
mkdir -p /etc/prometheus/alerts
cp alerts/*.yaml /etc/prometheus/alerts/

# Copy Alertmanager config
cp alertmanager.yaml /etc/alertmanager/alertmanager.yml
```

### 3. Start Alertmanager

```bash
# Start Alertmanager (listens on :9093)
alertmanager --config.file=/etc/alertmanager/alertmanager.yml
```

### 4. Start Prometheus

```bash
# Start Prometheus (listens on :9090)
prometheus --config.file=/etc/prometheus/prometheus.yml
```

### 5. Verify Setup

Open your browser:
- Prometheus UI: http://localhost:9090
- Alertmanager UI: http://localhost:9093
- Check targets: http://localhost:9090/targets
- Check alerts: http://localhost:9090/alerts

## Service Discovery

Prometheus can discover spawn instances using three methods:

### 1. EC2 Service Discovery (Recommended)

Automatically discovers all EC2 instances with the tag `spawn:metrics-enabled=true`.

**Requirements:**
- AWS credentials with `ec2:DescribeInstances` permission
- Instances must have security groups allowing TCP :9090 from Prometheus server

**Configuration:**
Already configured in `prometheus.yaml` under `spawn-ec2` job. Add additional regions as needed.

### 2. File-Based Service Discovery

Generate target files from spawn inventory:

```bash
# Create targets directory
mkdir -p /etc/prometheus/targets

# Generate targets.json from spawn inventory
spawn list --json | jq '[.instances[] | select(.metrics_enabled) | {
  targets: [.private_ip + ":9090"],
  labels: {
    instance_id: .instance_id,
    region: .region,
    instance_type: .instance_type,
    spot: (.spot | tostring)
  }
}]' > /etc/prometheus/targets/spawn-instances.json
```

Refresh this file periodically (e.g., every minute via cron).

### 3. Static Configuration

Manually add instance IPs to `prometheus.yaml`:

```yaml
scrape_configs:
  - job_name: 'spawn-instances'
    static_configs:
      - targets:
          - '10.0.1.100:9090'
          - '10.0.1.101:9090'
        labels:
          region: 'us-east-1'
```

## Alert Rules

26 alert rules organized into 4 categories:

### Instance Lifecycle (6 rules)
- `InstanceHighIdleTime` - Idle > 30 minutes
- `InstanceTTLExpiringSoon` - TTL < 5 minutes
- `InstanceIdleTimeoutExpiringSoon` - Idle timeout < 5 minutes
- `SpotInstanceInterruptionWarning` - Spot instance detected
- `InstanceNoActivity` - No activity for 2+ hours
- `InstanceRecentlyStarted` - Instance started < 5 minutes ago

### Cost Management (6 rules)
- `HighCostInstance` - Instance > $5/hour
- `DailyCostBudgetExceeded` - Total cost > $200
- `CostTrendingUp` - Cost 50%+ higher than 24h ago
- `HighRegionalCost` - Regional cost > $50
- `IdleHighCostInstance` - Idle instance > $2/hour
- `CostForecastExceeded` - Forecasted cost > budget

### Capacity (6 rules)
- `HighFleetSize` - Fleet size > 100 instances
- `LowSpotAvailability` - < 20% spot instances
- `RegionalCapacityImbalance` - One region has 3x average
- `JobArraySizeAnomaly` - Job array > 50 instances
- `FleetGrowthAnomaly` - Fleet doubled in 1 hour
- `ProviderImbalance` - EC2 10x local instances

### Performance (8 rules)
- `HighCPUUsage` - CPU > 95% for 5 minutes
- `HighMemoryUsage` - Memory > 90% for 5 minutes
- `GPUHighUtilization` - GPU > 95% for 10 minutes
- `GPUHighTemperature` - GPU > 80°C for 5 minutes
- `HighNetworkThroughput` - Network > 100MB/s for 10 minutes
- `HighDiskIO` - Disk I/O > 50MB/s for 10 minutes
- `FleetAverageCPUHigh` - Fleet avg CPU > 80% for 10 minutes
- `RegionalPerformanceDegradation` - Regional avg CPU > 90% for 15 minutes

## Alert Routing

Alertmanager routes all alerts to the spawn webhook bridge:

```
Prometheus → Alertmanager → Webhook Bridge → Spawn Alert System
                                              ↓
                                    SNS / Slack / Webhook
```

The webhook bridge runs on `http://localhost:8080/alertmanager/webhook` and converts Alertmanager alerts to spawn's format.

### Starting the Bridge

```bash
# The bridge is started automatically by spored if alerting is configured
# Or run standalone:
spawn alerting-bridge --listen :8080
```

## Customization

### Adjust Alert Thresholds

Edit alert rule files in `alerts/` directory:

```yaml
# Example: Change CPU threshold from 95% to 90%
- alert: HighCPUUsage
  expr: spawn_cpu_usage_percent > 90  # Changed from 95
  for: 5m
```

Reload Prometheus configuration:
```bash
curl -X POST http://localhost:9090/-/reload
```

### Add New Alert Rules

Create a new YAML file in `alerts/` directory:

```yaml
groups:
  - name: my_custom_alerts
    interval: 60s
    rules:
      - alert: MyCustomAlert
        expr: my_metric > threshold
        for: 5m
        labels:
          severity: warning
          component: spawn
          category: custom
        annotations:
          summary: "Alert summary"
          description: "Alert description"
```

Add to `prometheus.yaml`:
```yaml
rule_files:
  - "alerts/my-custom-alerts.yaml"
```

### Change Alert Routing

Edit `alertmanager.yaml` to add new receivers:

```yaml
receivers:
  - name: 'pagerduty'
    pagerduty_configs:
      - service_key: 'YOUR_KEY'

routes:
  - match:
      severity: critical
    receiver: 'pagerduty'
    continue: true
```

## Troubleshooting

### No Targets Discovered

**EC2 Service Discovery:**
- Check AWS credentials: `aws ec2 describe-instances`
- Verify tag exists: `spawn:metrics-enabled=true`
- Check Prometheus logs for discovery errors

**File-Based Discovery:**
- Verify targets file exists: `cat /etc/prometheus/targets/*.json`
- Check file syntax: `jq . /etc/prometheus/targets/spawn-instances.json`

### Targets Down

- Check instance security groups allow TCP :9090
- Verify spored metrics server is running: `curl http://instance-ip:9090/health`
- Check instance tags: `spawn:metrics-enabled=true`

### Alerts Not Firing

- Check alert rules syntax: `promtool check rules alerts/*.yaml`
- Verify metrics exist: http://localhost:9090/graph?g0.expr=spawn_cpu_usage_percent
- Check evaluation: http://localhost:9090/alerts

### Webhook Not Receiving Alerts

- Verify Alertmanager can reach webhook: `curl http://localhost:8080/health`
- Check Alertmanager logs for webhook errors
- Test webhook manually:
```bash
curl -X POST http://localhost:8080/alertmanager/webhook \
  -H "Content-Type: application/json" \
  -d '{"alerts":[{"status":"firing","labels":{"alertname":"test"}}]}'
```

## Production Deployment

### High Availability

Run multiple Prometheus and Alertmanager instances:

```yaml
# prometheus.yaml - add clustering
alerting:
  alertmanagers:
    - static_configs:
        - targets:
            - alertmanager-1:9093
            - alertmanager-2:9093
            - alertmanager-3:9093
```

```bash
# Start Alertmanager cluster
alertmanager --config.file=alertmanager.yml \
  --cluster.peer=alertmanager-2:9094 \
  --cluster.peer=alertmanager-3:9094
```

### Security

**Enable TLS:**
```yaml
# prometheus.yaml
scrape_configs:
  - job_name: 'spawn-fleet'
    scheme: https
    tls_config:
      ca_file: /path/to/ca.crt
      cert_file: /path/to/client.crt
      key_file: /path/to/client.key
```

**Enable Authentication:**
```yaml
# prometheus.yaml
scrape_configs:
  - job_name: 'spawn-fleet'
    basic_auth:
      username: 'prometheus'
      password: 'secret'
```

### Long-Term Storage

Configure remote write to long-term storage:

```yaml
# prometheus.yaml
remote_write:
  - url: "https://prometheus-storage.example.com/api/v1/write"
    basic_auth:
      username: "your-username"
      password: "your-password"
```

Popular options:
- Grafana Cloud
- Thanos
- Cortex
- VictoriaMetrics

## See Also

- [Prometheus Metrics Reference](../../docs/reference/metrics.md)
- [Alerting Setup Guide](../../docs/how-to/prometheus-alerting.md)
- [Grafana Dashboards](../grafana/README.md)
- [Spawn Monitoring Overview](../../MONITORING.md)
