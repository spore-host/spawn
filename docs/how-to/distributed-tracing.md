# Distributed Tracing

This guide explains how to enable and use OpenTelemetry distributed tracing in spawn.

## Overview

As of v0.19.0, spawn supports distributed tracing with OpenTelemetry. This enables:

- End-to-end visibility across AWS SDK calls
- Performance profiling and bottleneck identification
- Distributed debugging across hybrid compute
- Integration with AWS X-Ray or Jaeger

## Enabling Tracing

### EC2 Instances

Enable tracing using EC2 tags when launching:

```bash
spawn launch \
  --instance-type t3.micro \
  --tag spawn:tracing-enabled=true \
  --tag spawn:tracing-exporter=xray \
  --tag spawn:tracing-sampling=0.1
```

### Local Instances

Edit `/etc/spawn/local.yaml`:

```yaml
observability:
  tracing:
    enabled: true
    exporter: xray      # xray, stdout
    sampling_rate: 0.1  # 10% sampling
    endpoint: ""        # Optional custom endpoint
```

## Configuration

| Tag/Config | Default | Description |
|------------|---------|-------------|
| `spawn:tracing-enabled` / `enabled` | `false` | Enable tracing |
| `spawn:tracing-exporter` / `exporter` | `xray` | Exporter type |
| `spawn:tracing-sampling` / `sampling_rate` | `0.1` | Sampling rate (0.0-1.0) |
| `spawn:tracing-endpoint` / `endpoint` | `""` | Custom endpoint (optional) |

## Exporters

### AWS X-Ray (Production)

Best for production AWS workloads:

```yaml
observability:
  tracing:
    enabled: true
    exporter: xray
    sampling_rate: 0.1  # 10% to control costs
```

**View traces:**
1. Open AWS X-Ray Console
2. Navigate to Service Map or Traces
3. Filter by service name: `spored`

**Requirements:**
- IAM permissions: `xray:PutTraceSegments`
- X-Ray daemon not required (uses API directly)

### Stdout (Development)

For local development and debugging:

```yaml
observability:
  tracing:
    enabled: true
    exporter: stdout
    sampling_rate: 1.0  # 100% for debugging
```

Traces printed to stderr in JSON format.

## Instrumented Operations

### AWS SDK Calls

All AWS SDK operations are automatically traced:

- **EC2:** DescribeInstances, TerminateInstances, RunInstances
- **DynamoDB:** GetItem, PutItem, Query, UpdateItem
- **S3:** GetObject, PutObject, ListObjects
- **SQS:** SendMessage, ReceiveMessage, DeleteMessage
- **Lambda:** Invoke

### Span Attributes

Each span includes:
- `aws.service` - AWS service name (e.g., "ec2")
- `aws.operation` - Operation name
- `aws.region` - AWS region
- `aws.request_id` - AWS request ID
- `spawn.instance_id` - Instance ID
- `spawn.job_array_id` - Job array ID (if applicable)
- `error` - true/false

## Sampling

Control sampling rate to balance visibility and cost:

- **1.0** - 100% sampling (development only)
- **0.1** - 10% sampling (recommended for production)
- **0.01** - 1% sampling (high-volume production)

Sampling is consistent across distributed traces (head-based).

## Performance Impact

- **CPU:** <1% overhead with 10% sampling
- **Memory:** ~10MB for trace buffers
- **Network:** Batched uploads, minimal impact
- **Latency:** <5ms per traced operation

## Example Use Cases

### Performance Profiling

Find slow operations:

```
1. Enable tracing with 100% sampling
2. Execute workload
3. View trace in X-Ray console
4. Identify bottlenecks in Service Map
```

### Debugging Failures

Trace failed operations:

```
1. Enable tracing
2. Reproduce failure
3. Search X-Ray for error spans
4. View complete error context
```

### Distributed Debugging

Trace across job arrays:

```
1. Enable tracing on all instances
2. Trace context propagated via DynamoDB
3. View complete flow across instances
4. Identify coordination issues
```

## Troubleshooting

### No traces in X-Ray

1. Check IAM permissions: `xray:PutTraceSegments`
2. Verify tracing enabled: `spored config get tracing-enabled`
3. Check sampling: May need higher rate
4. Check spored logs for errors

### High costs

1. Lower sampling rate (0.01 = 1%)
2. Enable only for specific instances
3. Use time-based enabling (tags)

### Missing spans

1. Increase sampling rate temporarily
2. Check context propagation
3. Verify AWS SDK instrumentation

## Security Considerations

**IAM Permissions:**
```json
{
  "Effect": "Allow",
  "Action": [
    "xray:PutTraceSegments",
    "xray:PutTelemetryRecords"
  ],
  "Resource": "*"
}
```

**Data Privacy:**
- Span attributes may contain sensitive data
- Filter attributes in exporter configuration
- Use sampling to limit data volume

## Next Steps

- [Metrics Reference](../reference/metrics.md) - Related metrics
- [Grafana Dashboards](./grafana-dashboards.md) - Visualize traces
- [Prometheus Alerting](./prometheus-alerting.md) - Trace-based alerts
