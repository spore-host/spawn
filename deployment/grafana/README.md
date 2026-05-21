# Grafana Dashboards for Spawn

Pre-built Grafana dashboards for monitoring spawn instances.

## Dashboards

### 1. Instance Overview
**File:** `dashboards/instance-overview.json`

Detailed view of a single instance:
- Real-time metrics (CPU, memory, network)
- Idle state and duration
- TTL countdown
- Terminal and user activity

**Use Case:** Deep-dive into specific instance behavior

### 2. Fleet Monitoring
**File:** `dashboards/fleet-monitoring.json`

Overview of all instances:
- Total instance count
- Idle instances
- Running cost
- Instance list with filters
- Distribution by region/provider

**Use Case:** Fleet-wide monitoring and capacity planning

### 3. Cost Tracking
**File:** `dashboards/cost-tracking.json`

Cost analysis and forecasting:
- Total running cost
- Hourly cost rate
- Cost over time
- Cost breakdown by region
- Top 10 costliest instances

**Use Case:** Cost optimization and budget tracking

### 4. Hybrid Compute
**File:** `dashboards/hybrid-compute.json`

EC2 + Local instance coordination:
- Provider distribution
- Job array status
- Cross-provider metrics
- Hybrid instance list

**Use Case:** Hybrid compute monitoring

## Quick Start

### Option 1: Docker Compose

```bash
cd deployment/grafana
docker-compose up -d
```

Access Grafana at http://localhost:3000 (admin/admin)

### Option 2: Manual Import

1. Install Grafana: https://grafana.com/grafana/download
2. Add Prometheus data source (Settings → Data Sources)
   - URL: http://localhost:9090
   - Access: Server (default)
3. Import dashboards (Dashboards → Import)
   - Upload JSON files from `dashboards/` directory
   - Select Prometheus data source

### Option 3: Provisioning

Copy provisioning configs:

```bash
# Copy provisioning configs
sudo cp provisioning/*.yaml /etc/grafana/provisioning/datasources/
sudo cp provisioning/dashboards.yaml /etc/grafana/provisioning/dashboards/

# Copy dashboards
sudo mkdir -p /etc/grafana/provisioning/dashboards/spawn
sudo cp dashboards/*.json /etc/grafana/provisioning/dashboards/spawn/

# Restart Grafana
sudo systemctl restart grafana-server
```

## Requirements

- Grafana 10.0+
- Prometheus data source configured
- spawn instances with metrics enabled

## Dashboard Variables

### Instance Overview
- **$instance_id** - Select specific instance

### Fleet Monitoring
- **$region** - Filter by region (multi-select)

### Cost Tracking
- **$region** - Filter by region (multi-select)

## Customization

All dashboards are editable. Common customizations:

1. **Thresholds:** Adjust color thresholds for your use case
2. **Refresh Rate:** Change from 30s to suit your needs
3. **Time Range:** Adjust default time range
4. **Panels:** Add/remove panels as needed

## Troubleshooting

### No Data Appearing

1. Check Prometheus scraping:
   ```bash
   curl http://localhost:9090/api/v1/targets
   ```

2. Verify metrics endpoint:
   ```bash
   curl http://instance-ip:9090/metrics
   ```

3. Check data source connection in Grafana

### Panels Show "N/A"

- Metric may not be available (e.g., GPU on non-GPU instance)
- Check metric exists in Prometheus: http://localhost:9090/graph

### Slow Dashboard Loading

- Reduce time range
- Increase scrape interval
- Use query optimization (rate, avg)

## Screenshots

See `docs/how-to/grafana-dashboards.md` for dashboard screenshots.

## Support

For issues or questions:
- GitHub: https://github.com/spore-host/spore-host/issues
- Documentation: docs/how-to/grafana-dashboards.md
