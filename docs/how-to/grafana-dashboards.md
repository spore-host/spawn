# Grafana Dashboards

This guide explains how to import and use pre-built Grafana dashboards for spawn.

## Overview

Spawn includes 4 pre-built Grafana dashboards:

1. **Instance Overview** - Single instance drill-down
2. **Fleet Monitoring** - All instances overview
3. **Cost Tracking** - Cost analysis and forecasting
4. **Hybrid Compute** - EC2 + Local instance coordination

## Prerequisites

- Grafana 10.0+ installed
- Prometheus configured and scraping spawn metrics
- Spawn instances with metrics enabled

## Installation

### Method 1: Docker Compose (Recommended)

Create `docker-compose.yml`:

```yaml
version: '3.8'

services:
  prometheus:
    image: prom/prometheus:latest
    ports:
      - "9090:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
      - prometheus-data:/prometheus
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--storage.tsdb.path=/prometheus'

  grafana:
    image: grafana/grafana:latest
    ports:
      - "3000:3000"
    volumes:
      - grafana-data:/var/lib/grafana
      - ./deployment/grafana/provisioning:/etc/grafana/provisioning
      - ./deployment/grafana/dashboards:/etc/grafana/provisioning/dashboards/spawn
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=admin
      - GF_USERS_ALLOW_SIGN_UP=false
    depends_on:
      - prometheus

volumes:
  prometheus-data:
  grafana-data:
```

Start services:

```bash
docker-compose up -d
```

Access Grafana at http://localhost:3000 (admin/admin)

### Method 2: Manual Import

1. **Add Prometheus Data Source**
   - Navigate to Configuration → Data Sources
   - Click "Add data source"
   - Select Prometheus
   - URL: `http://localhost:9090`
   - Click "Save & Test"

2. **Import Dashboards**
   - Navigate to Dashboards → Import
   - Click "Upload JSON file"
   - Select dashboard file from `deployment/grafana/dashboards/`
   - Choose Prometheus data source
   - Click "Import"

3. **Repeat for All Dashboards**
   - Instance Overview
   - Fleet Monitoring
   - Cost Tracking
   - Hybrid Compute

### Method 3: Grafana Provisioning

For automated deployment:

```bash
# Copy provisioning configs
sudo cp deployment/grafana/provisioning/*.yaml \
  /etc/grafana/provisioning/datasources/

sudo cp deployment/grafana/provisioning/dashboards.yaml \
  /etc/grafana/provisioning/dashboards/

# Copy dashboards
sudo mkdir -p /etc/grafana/provisioning/dashboards/spawn
sudo cp deployment/grafana/dashboards/*.json \
  /etc/grafana/provisioning/dashboards/spawn/

# Restart Grafana
sudo systemctl restart grafana-server
```

## Dashboard Guide

### 1. Instance Overview

**Purpose:** Detailed metrics for a single instance

**Features:**
- Uptime, CPU, memory, TTL remaining
- CPU and network usage over time
- Idle state and duration
- Active terminals and logged-in users

**How to Use:**
1. Select instance from **$instance_id** dropdown
2. View real-time metrics in stat panels
3. Analyze trends in time series graphs
4. Monitor idle state for cost optimization

**Key Panels:**
- **Uptime:** How long instance has been running
- **CPU Usage:** Current CPU percentage with gauge
- **Memory Usage:** Current memory percentage
- **TTL Remaining:** Time until auto-termination
- **CPU Over Time:** Historical CPU usage
- **Network Throughput:** RX/TX bytes per second
- **Idle State:** Active or Idle status
- **Active Terminals:** Number of open terminal sessions

**Use Cases:**
- Debugging performance issues
- Monitoring specific workload
- Checking idle instances before termination

### 2. Fleet Monitoring

**Purpose:** Overview of all running instances

**Features:**
- Total instances, idle count, running cost
- Instance list with sortable columns
- Distribution by region and provider
- Average CPU by region
- Instance count over time

**How to Use:**
1. Filter by region using **$region** dropdown (multi-select)
2. Review total counts in stat panels
3. Sort instance list by CPU, uptime, etc.
4. Analyze distribution pie charts
5. Monitor trends over time

**Key Panels:**
- **Total Instances:** Current instance count
- **Idle Instances:** How many are idle
- **Total Running Cost:** Estimated cost to date
- **Expiring Soon:** Instances with <5min TTL
- **Instance List:** Sortable table with all instances
- **Instances by Region/Provider:** Distribution pie charts
- **Average CPU by Region:** Regional performance
- **Instance Count Over Time:** Fleet growth trends

**Use Cases:**
- Fleet capacity planning
- Identifying idle resources
- Regional distribution analysis
- Cost awareness

### 3. Cost Tracking

**Purpose:** Cost analysis and optimization

**Features:**
- Total running cost
- Hourly cost rate
- Forecasted daily cost
- Cost trends over time
- Cost breakdown by region
- Top 10 costliest instances

**How to Use:**
1. Filter by region using **$region** dropdown
2. Monitor total and hourly costs
3. Review cost trends for anomalies
4. Identify top cost drivers
5. Export data for budgeting

**Key Panels:**
- **Total Running Cost:** Cumulative cost
- **Hourly Cost Rate:** Current $/hour
- **Forecasted Daily Cost:** Next 24h projection
- **Cost Over Time:** Historical cost trends
- **Cost by Region:** Stacked area chart
- **Cost Distribution:** Regional pie chart
- **Top 10 Costliest Instances:** Sorted table

**Use Cases:**
- Budget monitoring and forecasting
- Cost optimization opportunities
- Regional cost comparison
- Identifying expensive workloads

### 4. Hybrid Compute

**Purpose:** EC2 + Local instance coordination

**Features:**
- Provider distribution (EC2 vs Local)
- Provider count over time
- Job array status
- Average CPU by provider
- Idle rate comparison
- Unified instance list

**How to Use:**
1. View EC2/Local distribution
2. Monitor job array sizes
3. Compare performance across providers
4. Analyze idle rates
5. Review hybrid instance list

**Key Panels:**
- **Instance Distribution:** EC2 vs Local pie chart
- **Provider Count Over Time:** Stacked area chart
- **EC2/Local Instances:** Individual counts
- **Job Arrays:** Number of active arrays
- **Total Array Size:** Sum of all array sizes
- **Average CPU by Provider:** Performance comparison
- **Idle Rate by Provider:** Utilization comparison
- **All Instances:** Unified table view

**Use Cases:**
- Hybrid compute monitoring
- Job array coordination
- Provider performance comparison
- Capacity planning across environments

## Dashboard Variables

Variables allow filtering and drill-down:

### $instance_id (Instance Overview)
- Single-select dropdown
- Lists all instances with metrics
- Updates all panels dynamically

### $region (Fleet Monitoring, Cost Tracking)
- Multi-select dropdown with "All" option
- Filter by one or more regions
- Applies to all panels

**Tip:** Use "All" to see complete fleet, then filter to specific regions for analysis

## Customization

### Editing Dashboards

1. Open dashboard
2. Click gear icon (⚙️) → Dashboard settings
3. Make changes:
   - Add/remove panels
   - Adjust thresholds
   - Change colors
   - Modify queries

4. Click "Save dashboard"

### Common Customizations

**Change Refresh Rate:**
- Top-right dropdown → Select interval (10s, 30s, 1m, etc.)

**Adjust Time Range:**
- Top-right time picker → Select preset or custom range

**Threshold Colors:**
- Edit panel → Field tab → Thresholds
- Adjust values and colors

**Panel Layout:**
- Enter edit mode
- Drag panels to reposition
- Resize by dragging corners

### Creating Custom Panels

1. Click "Add panel" → "Add an empty panel"
2. Configure query:
   ```promql
   spawn_cpu_usage_percent{instance_id=~"$instance_id"}
   ```
3. Select visualization type
4. Configure display options
5. Click "Apply"

## Alerting

Grafana supports alerting based on metrics:

1. **Create Alert Rule:**
   - Dashboard → Panel → Alert tab
   - Define condition (e.g., CPU > 90%)
   - Set evaluation interval
   - Configure notification channel

2. **Notification Channels:**
   - Alerting → Notification channels
   - Add: Email, Slack, PagerDuty, Webhook

3. **Example Alert:**
   ```
   Name: High CPU Usage
   Condition: avg() OF query(A, 5m) IS ABOVE 90
   Notification: slack-ops-channel
   ```

## Performance Tips

### Slow Dashboard Loading

**Reduce Time Range:**
- Use shorter ranges (1h, 6h) for detailed analysis
- Use longer ranges (24h, 7d) for trends

**Optimize Queries:**
- Use `rate()` for counters instead of raw values
- Use `avg by (label)` to reduce cardinality
- Avoid `*` in label matchers

**Increase Scrape Interval:**
- Edit Prometheus config: `scrape_interval: 60s`
- Trade frequency for performance

### High Cardinality

If too many instances:

1. Use label filters in variables
2. Create separate dashboards per environment
3. Use aggregate functions (avg, sum)
4. Increase Prometheus retention compression

## Troubleshooting

### No Data Appearing

**Check Prometheus Scraping:**
```bash
# View Prometheus targets
open http://localhost:9090/targets

# Check metrics endpoint
curl http://instance-ip:9090/metrics | grep spawn_
```

**Verify Data Source:**
- Grafana → Configuration → Data Sources
- Click "Test" button
- Should show "Data source is working"

**Check Time Range:**
- Ensure time range includes when instances were running
- Try "Last 24 hours"

### Panels Show "N/A"

**Metric Not Available:**
- Some metrics only exist on certain instances (e.g., GPU)
- Check Prometheus query browser: http://localhost:9090/graph

**Query Error:**
- Edit panel → Check query syntax
- Look for red errors in query editor

### Dashboard Import Fails

**Version Mismatch:**
- Ensure Grafana 10.0+
- Update Grafana if needed

**Missing Data Source:**
- Import will prompt for data source
- Select Prometheus from dropdown

## Best Practices

1. **Start with Fleet Monitoring**
   - Get overview before drilling down
   - Use it as your main dashboard

2. **Set Up Alerts**
   - High CPU (>90%)
   - TTL expiring soon (<5min)
   - High cost (custom threshold)

3. **Regular Review**
   - Check Cost Tracking daily
   - Review Fleet Monitoring for idle instances
   - Use Instance Overview for troubleshooting

4. **Customize for Your Needs**
   - Add organization-specific panels
   - Adjust thresholds for your workloads
   - Create views for different teams

5. **Export and Share**
   - Share dashboard URL with team
   - Export JSON for version control
   - Create snapshots for reporting

## Next Steps

- [Prometheus Alerting](./prometheus-alerting.md) - Set up advanced alerts
- [Metrics Reference](../reference/metrics.md) - Complete metric list
- [Distributed Tracing](./distributed-tracing.md) - Add tracing integration

## Additional Resources

- [Grafana Documentation](https://grafana.com/docs/)
- [PromQL Guide](https://prometheus.io/docs/prometheus/latest/querying/basics/)
- [Dashboard Best Practices](https://grafana.com/docs/grafana/latest/best-practices/)
