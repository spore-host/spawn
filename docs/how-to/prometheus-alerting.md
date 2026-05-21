# Prometheus Alerting Setup

This guide shows how to set up Prometheus Alertmanager with spawn's observability stack.

## Prerequisites

- Prometheus 2.45+ installed and running
- Alertmanager 0.26+ installed
- Spawn instances with metrics enabled (`spawn:metrics-enabled=true`)
- Existing spawn alert system configured (SNS/Slack/Webhook)

## Architecture

```
┌──────────────┐      ┌──────────────┐      ┌─────────────────┐      ┌─────────────┐
│  Prometheus  │─────▶│ Alertmanager │─────▶│ Webhook Bridge  │─────▶│ Spawn Alert │
│              │      │              │      │ (localhost:8080)│      │   System    │
│ :9090        │      │ :9093        │      │                 │      │             │
└──────────────┘      └──────────────┘      └─────────────────┘      └─────────────┘
                                                                              │
                                                                              ▼
                                                                      ┌───────────────┐
                                                                      │ SNS / Slack / │
                                                                      │   Webhook     │
                                                                      └───────────────┘
```

## Step 1: Configure Alert Rules

Alert rules are located in `deployment/prometheus/alerts/`:

```bash
deployment/prometheus/alerts/
├── instance-alerts.yaml      # Lifecycle alerts (6 rules)
├── cost-alerts.yaml          # Cost management (6 rules)
├── capacity-alerts.yaml      # Capacity alerts (6 rules)
└── performance-alerts.yaml   # Performance alerts (8 rules)
```

Copy to your Prometheus directory:

```bash
sudo mkdir -p /etc/prometheus/alerts
sudo cp deployment/prometheus/alerts/*.yaml /etc/prometheus/alerts/
```

Verify syntax:

```bash
promtool check rules /etc/prometheus/alerts/*.yaml
```

## Step 2: Configure Prometheus

Update `prometheus.yaml` to load alert rules:

```yaml
rule_files:
  - "alerts/instance-alerts.yaml"
  - "alerts/cost-alerts.yaml"
  - "alerts/capacity-alerts.yaml"
  - "alerts/performance-alerts.yaml"

alerting:
  alertmanagers:
    - static_configs:
        - targets:
            - localhost:9093
```

Copy configuration:

```bash
sudo cp deployment/prometheus/prometheus.yaml /etc/prometheus/prometheus.yml
```

Reload Prometheus:

```bash
# If running as systemd service
sudo systemctl reload prometheus

# Or send SIGHUP
kill -HUP $(pidof prometheus)

# Or use HTTP API
curl -X POST http://localhost:9090/-/reload
```

## Step 3: Configure Alertmanager

Copy Alertmanager configuration:

```bash
sudo cp deployment/prometheus/alertmanager.yaml /etc/alertmanager/alertmanager.yml
```

The configuration routes all alerts to spawn's webhook bridge:

```yaml
receivers:
  - name: 'spawn-webhook'
    webhook_configs:
      - url: 'http://localhost:8080/alertmanager/webhook'
        send_resolved: true
        max_alerts: 0

route:
  receiver: 'spawn-webhook'
  group_by: ['alertname', 'cluster', 'service']
  group_wait: 10s
  group_interval: 10s
  repeat_interval: 12h
```

Reload Alertmanager:

```bash
# If running as systemd service
sudo systemctl reload alertmanager

# Or send SIGHUP
kill -HUP $(pidof alertmanager)

# Or use HTTP API
curl -X POST http://localhost:9093/-/reload
```

## Step 4: Start Webhook Bridge

The webhook bridge converts Alertmanager alerts to spawn's format.

### Option A: Run with spored (Automatic)

Configure in instance tags:

```bash
spawn launch --instance-type t3.micro \
  --tag spawn:metrics-enabled=true \
  --tag spawn:alerting-bridge=true \
  --tag spawn:alerting-bind=0.0.0.0:8080
```

Or in local config:

```yaml
observability:
  alerting:
    bridge_enabled: true
    bind_address: "0.0.0.0:8080"
```

The bridge starts automatically when spored launches.

### Option B: Run Standalone

```bash
# Build and run the bridge
go run cmd/spawn-alerting-bridge/main.go \
  --listen :8080 \
  --alert-config /path/to/alert-config.yaml
```

Verify bridge is running:

```bash
curl http://localhost:8080/health
# Should return: OK
```

## Step 5: Test Alert Flow

### Test Alert Rule

Create a test alert:

```bash
# Trigger CPU alert by creating load
stress-ng --cpu 4 --timeout 10m --metrics-brief
```

Watch alert fire:

```bash
# Check Prometheus alerts page
open http://localhost:9090/alerts

# Or query via API
curl -s http://localhost:9090/api/v1/alerts | jq '.data.alerts[] | select(.labels.alertname=="HighCPUUsage")'
```

### Test Alertmanager

Send test alert to Alertmanager:

```bash
curl -X POST http://localhost:9093/api/v1/alerts \
  -H "Content-Type: application/json" \
  -d '[{
    "labels": {
      "alertname": "TestAlert",
      "severity": "warning",
      "category": "test"
    },
    "annotations": {
      "summary": "Test alert",
      "description": "This is a test"
    },
    "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"
  }]'
```

Check Alertmanager UI:

```bash
open http://localhost:9093
```

### Test Webhook Bridge

Send test webhook:

```bash
curl -X POST http://localhost:8080/alertmanager/webhook \
  -H "Content-Type: application/json" \
  -d '{
    "version": "4",
    "status": "firing",
    "alerts": [{
      "status": "firing",
      "labels": {
        "alertname": "TestAlert",
        "severity": "warning",
        "category": "test"
      },
      "annotations": {
        "summary": "Test alert from webhook",
        "description": "Testing the webhook bridge"
      },
      "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"
    }]
  }'
```

Check spawn alert system (SNS, Slack, etc.) for the test alert.

## Alert Categories

### Instance Lifecycle Alerts

Triggered by instance state changes:

- **InstanceHighIdleTime** - Instance idle for 30+ minutes
  - Severity: `warning`
  - Action: Consider terminating to save costs
  - Silence: Normal for interactive instances

- **InstanceTTLExpiringSoon** - TTL < 5 minutes remaining
  - Severity: `warning`
  - Action: Extend TTL if work is not complete
  - Silence: Not recommended

- **SpotInstanceInterruptionWarning** - Spot instance may be interrupted
  - Severity: `critical`
  - Action: Save state, prepare for interruption
  - Silence: Not recommended

### Cost Management Alerts

Triggered by cost anomalies:

- **DailyCostBudgetExceeded** - Total cost > $200
  - Severity: `critical`
  - Action: Review fleet, terminate unused instances
  - Silence: Increase budget in alert rule

- **HighCostInstance** - Instance > $5/hour
  - Severity: `warning`
  - Action: Verify instance type is correct
  - Silence: Normal for GPU instances

- **CostForecastExceeded** - Forecasted cost > budget
  - Severity: `warning`
  - Action: Scale down preemptively
  - Silence: Adjust forecast in alert rule

### Capacity Alerts

Triggered by scaling anomalies:

- **HighFleetSize** - Fleet > 100 instances
  - Severity: `warning`
  - Action: Review for unexpected scaling
  - Silence: Normal for large workloads

- **FleetGrowthAnomaly** - Fleet doubled in 1 hour
  - Severity: `warning`
  - Action: Verify scaling is intentional
  - Silence: During known scale-ups

### Performance Alerts

Triggered by resource exhaustion:

- **HighCPUUsage** - CPU > 95% for 5 minutes
  - Severity: `warning`
  - Action: Check for runaway processes
  - Silence: Normal for compute-intensive workloads

- **HighMemoryUsage** - Memory > 90% for 5 minutes
  - Severity: `warning`
  - Action: Check for memory leaks, scale up
  - Silence: Normal for memory-intensive workloads

## Customization

### Adjust Thresholds

Edit alert rule files:

```yaml
# Example: Change CPU threshold
- alert: HighCPUUsage
  expr: spawn_cpu_usage_percent > 90  # Changed from 95
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "CPU at {{ $value }}%"
```

Reload Prometheus:

```bash
curl -X POST http://localhost:9090/-/reload
```

### Change Severity Levels

```yaml
- alert: HighCostInstance
  expr: spawn_cost_per_hour_dollars > 10  # More strict
  for: 5m
  labels:
    severity: critical  # Escalated from warning
```

### Add Custom Annotations

```yaml
annotations:
  summary: "Instance {{ $labels.instance_id }} CPU at {{ $value }}%"
  description: "High CPU usage detected"
  runbook_url: "https://wiki.example.com/runbooks/high-cpu"
  dashboard_url: "https://grafana.example.com/d/instance-overview?var-instance_id={{ $labels.instance_id }}"
```

### Create Custom Alert Rules

Create `deployment/prometheus/alerts/custom-alerts.yaml`:

```yaml
groups:
  - name: custom_alerts
    interval: 60s
    rules:
      - alert: CustomMetricThreshold
        expr: my_custom_metric > 100
        for: 5m
        labels:
          severity: warning
          component: spawn
          category: custom
        annotations:
          summary: "Custom metric at {{ $value }}"
          description: "Custom alert description"
          instance_id: "{{ $labels.instance_id }}"
```

Add to `prometheus.yaml`:

```yaml
rule_files:
  - "alerts/custom-alerts.yaml"
```

## Alert Routing

### Route by Severity

```yaml
# alertmanager.yaml
route:
  receiver: 'spawn-webhook'
  routes:
    - match:
        severity: critical
      receiver: 'pagerduty'
      continue: true
    - match:
        severity: warning
      receiver: 'slack'
      continue: true
    - match:
        severity: info
      receiver: 'spawn-webhook'

receivers:
  - name: 'pagerduty'
    pagerduty_configs:
      - service_key: 'YOUR_KEY'
  - name: 'slack'
    slack_configs:
      - api_url: 'YOUR_WEBHOOK_URL'
        channel: '#alerts'
  - name: 'spawn-webhook'
    webhook_configs:
      - url: 'http://localhost:8080/alertmanager/webhook'
```

### Route by Category

```yaml
route:
  receiver: 'spawn-webhook'
  routes:
    - match:
        category: cost
      receiver: 'finance-team'
    - match:
        category: performance
      receiver: 'ops-team'
    - match:
        category: lifecycle
      receiver: 'spawn-webhook'
```

### Inhibit Lower Severity Alerts

Suppress warning alerts when critical alerts are firing:

```yaml
inhibit_rules:
  - source_match:
      severity: 'critical'
    target_match:
      severity: 'warning'
    equal: ['alertname', 'instance_id']
```

## Silencing Alerts

### Temporary Silence

Silence alerts during maintenance:

```bash
# Via Alertmanager UI
open http://localhost:9093/#/silences

# Or via API
curl -X POST http://localhost:9093/api/v2/silences \
  -H "Content-Type: application/json" \
  -d '{
    "matchers": [
      {"name": "instance_id", "value": "i-abc123", "isRegex": false}
    ],
    "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",
    "endsAt": "'$(date -u -d '+2 hours' +%Y-%m-%dT%H:%M:%SZ)'",
    "comment": "Maintenance window",
    "createdBy": "ops-team"
  }'
```

### Permanent Silence

Add matchers to Alertmanager config:

```yaml
# alertmanager.yaml
route:
  routes:
    - match:
        instance_id: 'i-persistent-123'
      receiver: 'null'
    - receiver: 'spawn-webhook'

receivers:
  - name: 'null'
  - name: 'spawn-webhook'
    # ...
```

## Troubleshooting

### Alerts Not Firing

**Check alert rule syntax:**
```bash
promtool check rules /etc/prometheus/alerts/*.yaml
```

**Verify metrics exist:**
```bash
curl -s 'http://localhost:9090/api/v1/query?query=spawn_cpu_usage_percent' | jq
```

**Check evaluation:**
```bash
# View alert state
open http://localhost:9090/alerts

# Check last evaluation time
curl -s http://localhost:9090/api/v1/rules | jq '.data.groups[] | .rules[] | select(.name=="HighCPUUsage")'
```

### Alerts Firing But Not Received

**Check Alertmanager status:**
```bash
curl -s http://localhost:9093/api/v2/status | jq
```

**View active alerts:**
```bash
curl -s http://localhost:9093/api/v2/alerts | jq
```

**Test webhook manually:**
```bash
curl -v -X POST http://localhost:8080/alertmanager/webhook \
  -H "Content-Type: application/json" \
  -d '{"alerts":[{"status":"firing","labels":{"alertname":"test"}}]}'
```

**Check bridge logs:**
```bash
# If running via spored
tail -f /var/log/spored.log | grep alerting

# If running standalone
journalctl -u spawn-alerting-bridge -f
```

### Alert Flapping

Alert repeatedly fires and resolves.

**Increase `for` duration:**
```yaml
- alert: HighCPUUsage
  expr: spawn_cpu_usage_percent > 95
  for: 10m  # Increased from 5m
```

**Add hysteresis:**
```yaml
- alert: HighCPUUsage
  expr: spawn_cpu_usage_percent > 95
  for: 5m
- alert: HighCPUUsageResolved
  expr: spawn_cpu_usage_percent < 80  # Lower threshold
  for: 5m
```

**Increase group_interval:**
```yaml
# alertmanager.yaml
route:
  group_interval: 5m  # Increased from 10s
```

### Too Many Alerts

**Increase repeat_interval:**
```yaml
# alertmanager.yaml
route:
  repeat_interval: 24h  # Increased from 12h
```

**Add inhibition rules:**
```yaml
inhibit_rules:
  - source_match:
      alertname: 'DailyCostBudgetExceeded'
    target_match:
      category: 'cost'
    equal: []
```

**Adjust severity:**
```yaml
# Downgrade to info
- alert: GPUHighUtilization
  labels:
    severity: info  # Changed from warning
```

## Production Considerations

### High Availability

Run multiple Alertmanager instances in cluster mode:

```bash
# Instance 1
alertmanager --config.file=alertmanager.yml \
  --cluster.listen-address=0.0.0.0:9094 \
  --cluster.peer=alertmanager-2:9094 \
  --cluster.peer=alertmanager-3:9094

# Instance 2
alertmanager --config.file=alertmanager.yml \
  --cluster.listen-address=0.0.0.0:9094 \
  --cluster.peer=alertmanager-1:9094 \
  --cluster.peer=alertmanager-3:9094

# Instance 3
alertmanager --config.file=alertmanager.yml \
  --cluster.listen-address=0.0.0.0:9094 \
  --cluster.peer=alertmanager-1:9094 \
  --cluster.peer=alertmanager-2:9094
```

Update Prometheus to use all instances:

```yaml
# prometheus.yaml
alerting:
  alertmanagers:
    - static_configs:
        - targets:
            - alertmanager-1:9093
            - alertmanager-2:9093
            - alertmanager-3:9093
```

### Persistent Storage

Configure Alertmanager data directory:

```bash
alertmanager --config.file=alertmanager.yml \
  --storage.path=/var/lib/alertmanager
```

### Security

**Enable TLS:**
```yaml
# alertmanager.yaml
tls_config:
  cert_file: /path/to/cert.pem
  key_file: /path/to/key.pem
```

**Enable authentication:**
```yaml
# alertmanager.yaml
http_config:
  basic_auth:
    username: admin
    password: secret
```

## See Also

- [Prometheus Metrics Reference](../reference/metrics.md)
- [Grafana Dashboards](../../deployment/grafana/README.md)
- [Monitoring Overview](../../MONITORING.md)
- [Prometheus Documentation](https://prometheus.io/docs/)
- [Alertmanager Documentation](https://prometheus.io/docs/alerting/latest/alertmanager/)
