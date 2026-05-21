# Metrics Reference

Complete reference for all Prometheus metrics exposed by spored.

## Instance Lifecycle

### `spawn_instance_uptime_seconds`
**Type:** Gauge
**Labels:** `instance_id`, `region`, `provider`
**Description:** Time in seconds since instance started

### `spawn_instance_start_time_seconds`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Unix timestamp when instance started

### `spawn_instance_spot`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Whether instance is a spot instance (1=yes, 0=no)

## Resource Usage

### `spawn_cpu_usage_percent`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** CPU usage percentage (0-100)

### `spawn_network_bytes_total`
**Type:** Counter
**Labels:** `instance_id`, `interface`, `direction`
**Description:** Network bytes transferred (direction: rx, tx)

### `spawn_disk_io_bytes_total`
**Type:** Counter
**Labels:** `instance_id`, `device`, `operation`
**Description:** Disk I/O bytes (operation: read, write)

### `spawn_gpu_utilization_percent`
**Type:** Gauge
**Labels:** `instance_id`, `gpu_index`
**Description:** GPU utilization percentage (0-100)

### `spawn_gpu_memory_used_bytes`
**Type:** Gauge
**Labels:** `instance_id`, `gpu_index`
**Description:** GPU memory used in bytes

### `spawn_gpu_memory_total_bytes`
**Type:** Gauge
**Labels:** `instance_id`, `gpu_index`
**Description:** GPU memory total in bytes

### `spawn_gpu_temperature_celsius`
**Type:** Gauge
**Labels:** `instance_id`, `gpu_index`
**Description:** GPU temperature in Celsius

### `spawn_gpu_power_watts`
**Type:** Gauge
**Labels:** `instance_id`, `gpu_index`
**Description:** GPU power usage in watts

### `spawn_memory_used_bytes`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Memory used in bytes

### `spawn_memory_total_bytes`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Memory total in bytes

## Idle Detection

### `spawn_idle_state`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Whether instance is idle (1=idle, 0=active)

### `spawn_idle_duration_seconds`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Time in seconds since last activity

### `spawn_active_terminals`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Number of active terminal sessions

### `spawn_logged_in_users`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Number of logged in users

## Timeouts

### `spawn_ttl_remaining_seconds`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Time remaining before TTL expiration (0 if disabled)

### `spawn_idle_timeout_remaining_seconds`
**Type:** Gauge
**Labels:** `instance_id`
**Description:** Time remaining before idle timeout (0 if disabled)

## Job Arrays

### `spawn_job_array_size`
**Type:** Gauge
**Labels:** `job_array_id`, `job_array_name`
**Description:** Size of job array

### `spawn_job_array_index`
**Type:** Gauge
**Labels:** `job_array_id`
**Description:** Index of this instance in job array

## Example Queries

### High CPU Usage
```promql
spawn_cpu_usage_percent > 90
```

### Idle Instances
```promql
spawn_idle_state == 1
```

### Instances Expiring Soon
```promql
spawn_ttl_remaining_seconds < 300
```

### GPU Memory Usage
```promql
spawn_gpu_memory_used_bytes / spawn_gpu_memory_total_bytes * 100
```

### Network Throughput
```promql
rate(spawn_network_bytes_total[5m])
```

### Active Instances
```promql
count(spawn_instance_uptime_seconds)
```

### Instances by Provider
```promql
count by (provider) (spawn_instance_uptime_seconds)
```

### Memory Usage Percentage
```promql
spawn_memory_used_bytes / spawn_memory_total_bytes * 100
```

## Label Reference

### `instance_id`
EC2 instance ID (e.g., `i-abc123`) or local hostname

### `region`
AWS region (e.g., `us-east-1`) or `local`

### `provider`
Provider type: `ec2` or `local`

### `interface`
Network interface name (e.g., `eth0`, `ens5`)

### `device`
Disk device name (e.g., `xvda`, `nvme0n1`)

### `gpu_index`
GPU index (0-based)

### `direction`
Network direction: `rx` (received) or `tx` (transmitted)

### `operation`
Disk operation: `read` or `write`

### `job_array_id`
Job array identifier (if part of job array)

### `job_array_name`
Job array name (if part of job array)

## Metric Types

### Gauge
Current value that can go up or down

### Counter
Cumulative value that only increases (use `rate()` for per-second)

## Collection Interval

Metrics are collected on demand when scraped. Internal monitoring runs every 60 seconds.
