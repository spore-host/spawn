# Prometheus Metrics

This guide explains how to enable and use Prometheus metrics in spawn.

## Overview

As of v0.19.0, spored exposes Prometheus-compatible metrics via an HTTP endpoint. This enables:

- Real-time fleet monitoring with Prometheus
- Custom dashboards in Grafana
- Historical analysis and cost tracking
- Integration with existing observability stacks

## Enabling Metrics

### EC2 Instances

Enable metrics using EC2 tags when launching:

```bash
spawn launch \
  --instance-type t3.micro \
  --tag spawn:metrics-enabled=true \
  --tag spawn:metrics-port=9090 \
  --tag spawn:metrics-bind=localhost
```

**Security Note:** By default, metrics bind to `localhost` for security. Only enable remote access if needed.

### Local Instances

Edit `/etc/spawn/local.yaml`:

```yaml
observability:
  metrics:
    enabled: true
    port: 9090
    path: /metrics
    bind: localhost  # Change to 0.0.0.0 for remote access
```

## Configuration

| Tag/Config | Default | Description |
|------------|---------|-------------|
| `spawn:metrics-enabled` / `enabled` | `false` | Enable metrics endpoint |
| `spawn:metrics-port` / `port` | `9090` | HTTP port |
| `spawn:metrics-path` / `path` | `/metrics` | HTTP path |
| `spawn:metrics-bind` / `bind` | `localhost` | Bind address |

## Endpoints

### `/metrics`
Prometheus text format metrics (primary endpoint).

```bash
curl http://localhost:9090/metrics
```

### `/health`
Health check endpoint (liveness probe).

```bash
curl http://localhost:9090/health
# Output: ok
```

### `/state`
JSON state dump (debugging).

```bash
curl http://localhost:9090/state
```

## Prometheus Configuration

Add to `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'spawn'
    static_configs:
      - targets:
          - 'instance-1.example.com:9090'
          - 'instance-2.example.com:9090'
    scrape_interval: 60s
```

### Dynamic Discovery (EC2)

Use EC2 service discovery:

```yaml
scrape_configs:
  - job_name: 'spawn-ec2'
    ec2_sd_configs:
      - region: us-east-1
        port: 9090
    relabel_configs:
      - source_labels: [__meta_ec2_tag_spawn_metrics_enabled]
        regex: true
        action: keep
```

## Grafana Integration

1. Add Prometheus as data source
2. Import dashboards from `deployment/grafana/dashboards/`
3. See `docs/how-to/grafana-dashboards.md` for details

## Security Considerations

**Bind Address:**
- `localhost` - Only accessible from instance (default, secure)
- `0.0.0.0` - Accessible from network (requires firewall)

**Firewall:**
```bash
# Allow only from Prometheus server
sudo ufw allow from 10.0.1.0/24 to any port 9090
```

**SSH Tunnel:**
```bash
# Access metrics via SSH tunnel
ssh -L 9090:localhost:9090 user@instance
curl http://localhost:9090/metrics
```

## Example Queries

### CPU Usage
```promql
spawn_cpu_usage_percent{instance_id="i-abc123"}
```

### Idle Instances
```promql
spawn_idle_state == 1
```

### Cost Per Hour
```promql
sum by (region) (spawn_cost_per_hour_dollars)
```

### TTL Expiring Soon
```promql
spawn_ttl_remaining_seconds < 300
```

## Troubleshooting

### Metrics not available
1. Check if enabled: `spored config get metrics-enabled`
2. Check spored logs: `journalctl -u spored -n 50`
3. Test locally: `curl http://localhost:9090/health`

### Connection refused
1. Check bind address (must be `0.0.0.0` for remote)
2. Check firewall/security groups
3. Verify port: `sudo lsof -i :9090`

### Empty metrics
1. Agent may still be initializing
2. Some metrics require activity (GPU, network)
3. Check `/state` endpoint for debugging

## Next Steps

- [Metrics Reference](../reference/metrics.md) - Complete metric list
- [Grafana Dashboards](./grafana-dashboards.md) - Pre-built dashboards
- [Prometheus Alerting](./prometheus-alerting.md) - Alert rules
