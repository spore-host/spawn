# spawnd Monitoring

The spawnd agent monitors instances and can auto-terminate when idle.

## Metrics Monitored

The spawnd agent monitors the following metrics to determine if an instance is idle:

1. **CPU Usage** - Threshold: <5% (configurable via `spawn:idle-cpu` tag)
2. **Network Traffic** - Threshold: ≤10KB/min
3. **Disk I/O** - Threshold: ≤100KB/min
4. **GPU Utilization** - Threshold: ≤5% (if nvidia-smi available)
5. **Active Terminals** - Checks `/dev/pts/` for active PTYs
6. **Logged-in Users** - Uses `who` command
7. **Recent User Activity** - Checks `wtmp` logs (last 5 minutes)

## Idle Detection

The system is considered **idle** when **ALL** conditions are met:
- CPU usage < threshold
- Network traffic ≤ threshold
- Disk I/O ≤ threshold
- GPU utilization ≤ threshold (if GPU present)
- No active terminals
- No logged-in users
- No recent user activity

If any single condition fails, the system is considered active.

## Configuration

Set monitoring behavior via EC2 instance tags when launching:

```bash
spawn launch --instance-type g5.xlarge \
  --idle-timeout 30m \               # Idle timeout duration
  --idle-cpu 5.0 \                   # CPU threshold (%)
  --hibernate-on-idle                # Hibernate instead of terminate
```

### Available Tags

- `spawn:idle-timeout` - Duration before terminating idle instance (e.g., "30m", "2h", "1d")
- `spawn:idle-cpu` - CPU usage percentage threshold (default: 5.0)
- `spawn:hibernate-on-idle` - Set to "true" to hibernate instead of terminate
- `spawn:ttl` - Maximum time-to-live regardless of activity
- `spawn:completion-tag` - Stop when specific tag appears

## Disk I/O Monitoring

The spawnd agent monitors disk I/O by reading `/proc/diskstats` and tracking:

### Monitored Device Types

- **xvd\*** - Xen virtual disks (e.g., xvda, xvdb)
- **nvme\*** - NVMe SSDs (e.g., nvme0n1, nvme1n1)
- **sd\*** - SCSI/SATA drives (e.g., sda, sdb)
- **vd\*** - virtio disks (e.g., vda, vdb)

### Partition Handling

The agent attempts to skip partitions to avoid double-counting:
- Partitions are identified by checking if device name ends with a digit and length > 4
- Examples: xvda1, nvme0n1p1, sdb2 are partitions
- Main devices: xvda, nvme0n1, sda are main block devices

**Note**: The current implementation has limitations with certain device naming schemes (e.g., nvme0n1 is incorrectly classified as a partition).

### Calculation

```
Total I/O = (sectors_read + sectors_written) × 512 bytes
Idle if: Total I/O ≤ 100KB per minute
```

## GPU Monitoring

GPU monitoring requires the `nvidia-smi` command-line tool.

### Detection

The agent checks for nvidia-smi availability at startup:
- If found: Monitors GPU utilization
- If not found: GPU monitoring returns 0% (system can still be idle based on other metrics)

### Query Method

```bash
nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits
```

### Multi-GPU Support

For systems with multiple GPUs:
- The agent queries all GPUs
- Returns the **maximum utilization** across all GPUs
- Example: GPU0=10%, GPU1=75%, GPU2=5% → Reports 75%

### Idle Threshold

GPU is considered idle when utilization ≤ 5%

## Debugging

Check if monitoring is working:

```bash
# SSH into instance
ssh into-instance

# Check spored status (if status command implemented)
sudo spored status
```

### Manual Metric Check

You can manually check metrics using the same methods spawnd uses:

```bash
# Check disk I/O
cat /proc/diskstats | awk '$3 ~ /^(xvd|nvme|sd|vd)/ {print $3, $6, $10}'

# Check GPU utilization (if nvidia-smi available)
nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits

# Check CPU usage
top -bn1 | grep "Cpu(s)"

# Check network traffic
cat /proc/net/dev

# Check logged-in users
who

# Check active terminals
ls /dev/pts/
```

## Use Cases

### Development Instances

Launch an instance that terminates after 30 minutes of inactivity:

```bash
spawn launch --instance-type m7i.large \
  --idle-timeout 30m \
  --name dev-instance
```

### Long-Running Tasks

Launch an instance for a long-running task with high TTL but idle detection:

```bash
spawn launch --instance-type c6a.xlarge \
  --ttl 24h \
  --idle-timeout 1h \
  --name batch-processing
```

The instance will terminate after either:
- 24 hours total (TTL)
- 1 hour of continuous idle time

### GPU Workloads

Launch a GPU instance that hibernates when idle:

```bash
spawn launch --instance-type g5.xlarge \
  --idle-timeout 15m \
  --hibernate-on-idle \
  --name ml-training
```

Hibernation preserves memory state and allows resuming work later.

### Cost-Conscious Development

Launch with aggressive idle detection to minimize costs:

```bash
spawn launch --instance-type t3.medium \
  --idle-timeout 10m \
  --idle-cpu 3.0 \
  --name quick-test
```

This terminates quickly after work is complete.

## Thresholds Explained

### Why These Thresholds?

- **CPU < 5%**: Allows for background system processes
- **Network ≤ 10KB/min**: Filters out periodic health checks and metrics
- **Disk I/O ≤ 100KB/min**: Allows for log rotation and system writes
- **GPU ≤ 5%**: Accounts for desktop compositing and minimal rendering

### Adjusting Thresholds

Currently, only CPU threshold is configurable via `--idle-cpu` flag:

```bash
spawn launch --instance-type m7i.large \
  --idle-timeout 30m \
  --idle-cpu 10.0  # More lenient CPU threshold
```

Network, disk, and GPU thresholds are hardcoded but could be made configurable in future versions.

## Monitoring Interval

The spawnd agent checks metrics every 60 seconds by default. This means:

- An instance must be **continuously idle** for the full idle-timeout duration
- Any activity resets the idle timer
- Short bursts of activity will prevent termination

## Limitations

### Partition Detection

The current partition detection logic has edge cases:
- **nvme0n1** (main device) is incorrectly skipped because it ends with '1'
- **sda1** (partition) is incorrectly included because length=4 is not >4
- **vda1** (partition) is incorrectly included for the same reason

This may result in slightly inaccurate disk I/O measurements but shouldn't significantly affect idle detection.

### GPU Detection

- Only NVIDIA GPUs are supported (requires nvidia-smi)
- AMD and Intel GPUs are not currently monitored
- GPU monitoring gracefully degrades if nvidia-smi is not available

### False Positives

The agent may incorrectly detect activity in some cases:
- Background system updates
- Cron jobs
- Log rotation
- System monitoring tools

Adjust thresholds if false positives are common in your environment.

## Safety Features

### Active User Protection

The agent will **never** terminate an instance with:
- Logged-in users (via `who`)
- Active terminal sessions (PTYs in `/dev/pts/`)
- Recent user activity (wtmp within last 5 minutes)

This prevents accidentally terminating instances while users are working.

### Spot Instance Interruption

For Spot instances, spawnd monitors the EC2 metadata service for interruption warnings and:
1. Sends notifications to logged-in users
2. Runs cleanup tasks
3. Gracefully terminates before AWS forcibly stops the instance

### Graceful Shutdown

When terminating, spawnd:
1. Cleans up DNS records
2. Notifies users (if any)
3. Allows cleanup hooks to run
4. Performs graceful shutdown

## Implementation Details

### File Locations

- **Disk stats**: `/proc/diskstats`
- **CPU stats**: `/proc/stat`
- **Network stats**: `/proc/net/dev`
- **PTY devices**: `/dev/pts/*`
- **User logs**: `/var/run/utmp`, `/var/log/wtmp`

### Error Handling

If the agent cannot read a metric (e.g., /proc files unavailable):
- The metric is assumed to be 0 (idle)
- Monitoring continues with remaining metrics
- Errors are logged but don't crash the agent

This ensures robustness in various environments.

## Prometheus Metrics (v0.19.0+)

Spawn now supports exposing metrics in Prometheus format via HTTP. This enables integration with modern observability stacks.

### Enabling Metrics

Enable metrics server via instance tags:

```bash
spawn launch --instance-type m7i.large \
  --tag spawn:metrics-enabled=true \
  --tag spawn:metrics-port=9090 \
  --tag spawn:metrics-bind=localhost
```

Or in local config (`~/.config/spawn/local-config.yaml`):

```yaml
observability:
  metrics:
    enabled: true
    port: 9090
    bind: "localhost"
```

### Metrics Endpoints

Once enabled, spored exposes:
- `http://localhost:9090/metrics` - Prometheus text format
- `http://localhost:9090/health` - Health check
- `http://localhost:9090/state` - JSON state dump

### Available Metrics

**Instance Lifecycle:**
- `spawn_instance_uptime_seconds` - Instance uptime
- `spawn_instance_start_time_seconds` - Instance start timestamp
- `spawn_instance_spot` - Spot instance indicator (0 or 1)
- `spawn_ttl_remaining_seconds` - TTL countdown
- `spawn_idle_timeout_remaining_seconds` - Idle timeout countdown

**Resource Usage:**
- `spawn_cpu_usage_percent` - Current CPU usage
- `spawn_memory_used_bytes` / `spawn_memory_total_bytes` - Memory metrics
- `spawn_network_bytes_total` - Network traffic (by interface)
- `spawn_disk_io_bytes_total` - Disk I/O (by device)
- `spawn_gpu_utilization_percent` - GPU utilization (by GPU index)
- `spawn_gpu_temperature_celsius` - GPU temperature

**Idle Detection:**
- `spawn_idle_state` - Current idle state (0=active, 1=idle)
- `spawn_idle_duration_seconds` - How long instance has been idle
- `spawn_active_terminals` - Number of active PTYs
- `spawn_logged_in_users` - Number of logged-in users

**Cost Tracking:**
- `spawn_cost_per_hour_dollars` - Hourly cost
- `spawn_estimated_cost_dollars` - Estimated cost since launch

**Job Arrays:**
- `spawn_job_array_size` - Total job array size
- `spawn_job_array_index` - This instance's index
- `spawn_job_array_peers_discovered` - Number of peers found

### Prometheus Integration

Configure Prometheus to scrape spawn instances:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'spawn-fleet'
    ec2_sd_configs:
      - region: us-east-1
        port: 9090
        filters:
          - name: tag:spawn:metrics-enabled
            values: ['true']
    relabel_configs:
      - source_labels: [__meta_ec2_instance_id]
        target_label: instance_id
```

See `deployment/prometheus/prometheus.yaml` for complete configuration.

### Grafana Dashboards

Pre-built Grafana dashboards are available in `deployment/grafana/dashboards/`:
- **instance-overview.json** - Single instance drill-down
- **fleet-monitoring.json** - Fleet-wide overview
- **cost-tracking.json** - Cost analysis and forecasting
- **hybrid-compute.json** - EC2 + local instance metrics

Import dashboards:
```bash
grafana-cli dashboard import deployment/grafana/dashboards/instance-overview.json
```

See `deployment/grafana/README.md` for setup instructions.

## OpenTelemetry Tracing (v0.19.0+)

Spawn supports distributed tracing with OpenTelemetry for debugging complex workloads.

### Enabling Tracing

Enable tracing via instance tags:

```bash
spawn launch --instance-type m7i.large \
  --tag spawn:tracing-enabled=true \
  --tag spawn:tracing-exporter=xray \
  --tag spawn:tracing-sampling=0.1
```

Or in local config:

```yaml
observability:
  tracing:
    enabled: true
    exporter: "xray"  # or "stdout"
    sampling_rate: 0.1
```

### Supported Exporters

- **xray** - AWS X-Ray (default, recommended for AWS)
- **stdout** - Console output (debugging)

### What Gets Traced

When tracing is enabled, spans are created for:
- AWS SDK calls (EC2, DynamoDB, S3, SQS, etc.)
- Queue operations
- Job execution
- Peer discovery

### Viewing Traces

**AWS X-Ray:**
```bash
# View traces in X-Ray console
open https://console.aws.amazon.com/xray/home

# Query traces via CLI
aws xray get-trace-summaries \
  --start-time $(date -u -d '10 minutes ago' +%s) \
  --end-time $(date -u +%s)
```

### Trace Context Propagation

Traces propagate across:
- Agent → Queue → Lambda flows
- Multi-instance job arrays
- Cross-service AWS SDK calls

## Alertmanager Integration (v0.19.0+)

Spawn supports Prometheus Alertmanager for advanced alerting beyond the existing SNS/Slack integration.

### Alert Rules

26 pre-built alert rules in `deployment/prometheus/alerts/`:

**Instance Lifecycle (6 rules):**
- InstanceHighIdleTime - Idle > 30 minutes
- InstanceTTLExpiringSoon - TTL < 5 minutes
- SpotInstanceInterruptionWarning
- InstanceNoActivity - No activity for 2+ hours

**Cost Management (6 rules):**
- DailyCostBudgetExceeded - Total cost > $200
- HighCostInstance - Instance > $5/hour
- CostForecastExceeded - Predictive cost alert
- IdleHighCostInstance - Idle instance > $2/hour

**Capacity (6 rules):**
- HighFleetSize - Fleet > 100 instances
- FleetGrowthAnomaly - Fleet doubled in 1 hour
- ProviderImbalance - EC2 vs local imbalance

**Performance (8 rules):**
- HighCPUUsage - CPU > 95% for 5 minutes
- HighMemoryUsage - Memory > 90% for 5 minutes
- GPUHighTemperature - GPU > 80°C
- FleetAverageCPUHigh - Fleet avg CPU > 80%

### Setup

1. **Install Prometheus and Alertmanager:**
```bash
brew install prometheus alertmanager
```

2. **Configure Prometheus:**
```bash
cp deployment/prometheus/prometheus.yaml /etc/prometheus/prometheus.yml
cp deployment/prometheus/alerts/*.yaml /etc/prometheus/alerts/
```

3. **Configure Alertmanager:**
```bash
cp deployment/prometheus/alertmanager.yaml /etc/alertmanager/alertmanager.yml
```

4. **Start services:**
```bash
prometheus --config.file=/etc/prometheus/prometheus.yml
alertmanager --config.file=/etc/alertmanager/alertmanager.yml
```

### Webhook Bridge

Alertmanager routes alerts through a webhook bridge to integrate with spawn's existing alert system:

```
Prometheus → Alertmanager → Webhook Bridge → SNS/Slack/Email
```

The bridge converts Prometheus alerts to spawn format and sends via configured channels.

### Documentation

- Complete setup: `docs/how-to/prometheus-alerting.md`
- Metrics reference: `docs/reference/metrics.md`
- Quick start: `deployment/prometheus/README.md`

## Testing

The monitoring logic is tested in `spawn/pkg/agent/monitoring_test.go` with tests for:

- Disk I/O parsing and threshold detection
- GPU utilization parsing and multi-GPU handling
- Partition detection logic
- Device type recognition
- Sector-to-byte conversion
- Idle detection with multiple conditions
- Real-world usage scenarios

Run tests:

```bash
cd spawn/pkg/agent
go test -v -run TestGetDiskIO
go test -v -run TestGetGPU
go test -v -run TestIsIdle
```
