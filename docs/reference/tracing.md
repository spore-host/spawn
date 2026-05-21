# Tracing Reference

Complete reference for OpenTelemetry distributed tracing in spawn.

## Span Naming Convention

Spans follow OpenTelemetry semantic conventions:

```
{service}.{operation}
aws.{service}.{operation}
```

**Examples:**
- `aws.ec2.DescribeInstances`
- `aws.dynamodb.GetItem`
- `spawn.agent.monitor`

## Standard Attributes

### AWS SDK Spans

All AWS SDK operations include:

| Attribute | Type | Description | Example |
|-----------|------|-------------|---------|
| `aws.service` | string | AWS service name | `ec2` |
| `aws.operation` | string | Operation name | `DescribeInstances` |
| `aws.region` | string | AWS region | `us-east-1` |
| `aws.request_id` | string | AWS request ID | `abc-123-def` |
| `http.status_code` | int | HTTP status | `200` |
| `error` | bool | Whether error occurred | `false` |

### Spawn-Specific Attributes

Custom attributes added by spawn:

| Attribute | Type | Description | Example |
|-----------|------|-------------|---------|
| `spawn.instance_id` | string | Instance ID | `i-abc123` |
| `spawn.provider` | string | Provider type | `ec2`, `local` |
| `spawn.job_array_id` | string | Job array ID | `ja-abc123` |
| `spawn.job_array_index` | int | Job array index | `5` |

## Trace Context Propagation

### Between Instances

Trace context propagated via DynamoDB for hybrid coordination:

```
Instance A → DynamoDB → Instance B
     ↓           ↓           ↓
  Span A  → Metadata → Child Span
```

**DynamoDB Attributes:**
- `traceparent` - W3C Trace Context format
- `tracestate` - Vendor-specific trace state

### SQS Messages

Trace context in message attributes:

```json
{
  "MessageAttributes": {
    "traceparent": {
      "StringValue": "00-trace-id-span-id-01",
      "DataType": "String"
    }
  }
}
```

## Span Lifecycle

### Creation

```go
ctx, span := tracer.Start(ctx, "operation-name")
defer span.End()
```

### Adding Attributes

```go
span.SetAttributes(
    attribute.String("key", "value"),
    attribute.Int("count", 42),
)
```

### Recording Errors

```go
if err != nil {
    span.RecordError(err)
    span.SetStatus(codes.Error, err.Error())
}
```

## Sampling Strategies

### Head-Based Sampling

Decision made at trace creation:

| Rate | Use Case | Cost Impact |
|------|----------|-------------|
| 1.0 | Development | Very high |
| 0.1 | Production | Low |
| 0.01 | High-volume | Very low |

### Consistent Sampling

All spans in a trace share same decision.

## Exporter Configuration

### X-Ray Exporter

```yaml
observability:
  tracing:
    enabled: true
    exporter: xray
    sampling_rate: 0.1
```

**Segment Format:**
```json
{
  "trace_id": "1-5f5a1234-abcdef1234567890",
  "id": "span-id-123",
  "name": "aws.ec2.DescribeInstances",
  "start_time": 1609459200.0,
  "end_time": 1609459201.5,
  "service": {
    "name": "spawn"
  },
  "metadata": {
    "spawn": {
      "instance_id": "i-abc123",
      "operation": "describe"
    }
  }
}
```

### Stdout Exporter

```yaml
observability:
  tracing:
    enabled: true
    exporter: stdout
    sampling_rate: 1.0
```

**Output Format:**
```json
{
  "trace_id": "abc123...",
  "span_id": "def456...",
  "name": "operation",
  "start": "2024-01-01T00:00:00Z",
  "end": "2024-01-01T00:00:01Z",
  "duration": "1s",
  "status": "OK",
  "attributes": {
    "key": "value"
  }
}
```

## Performance Characteristics

### Overhead

| Metric | Impact | Notes |
|--------|--------|-------|
| CPU | <1% | At 10% sampling |
| Memory | ~10MB | For trace buffers |
| Network | Minimal | Batched uploads |
| Latency | <5ms | Per traced operation |

### Batch Export

- **Batch Size:** 100 spans
- **Batch Timeout:** 5 seconds
- **Max Queue:** 2048 spans
- **Export Timeout:** 30 seconds

## Error Handling

### Span Errors

```go
if err != nil {
    span.RecordError(err)
    span.SetStatus(codes.Error, "operation failed")
    return err
}
```

### Export Failures

- Spans queued and retried
- Max retries: 3
- Backoff: exponential
- Drop policy: oldest first

## Security

### Data Sanitization

Sensitive data filtered from spans:

- Credentials
- API keys
- Passwords
- PII (if configured)

### IAM Permissions

Required for X-Ray:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "xray:PutTraceSegments",
        "xray:PutTelemetryRecords"
      ],
      "Resource": "*"
    }
  ]
}
```

## Best Practices

### When to Use Tracing

**Good:**
- Debugging performance issues
- Understanding distributed flows
- Profiling AWS SDK calls
- Production monitoring (low sampling)

**Avoid:**
- High-frequency operations (>1000 QPS)
- Sensitive operations (unless filtered)
- Development without sampling limits

### Attribute Naming

Follow OpenTelemetry conventions:

- Use namespaces: `spawn.`, `aws.`
- Lowercase with underscores: `instance_id`
- Be consistent across services

### Span Granularity

- One span per logical operation
- Avoid spans <1ms duration
- Group fine-grained operations

## Troubleshooting

### Common Issues

**No traces appear:**
- Check IAM permissions
- Verify exporter configuration
- Increase sampling rate
- Check logs for errors

**Incomplete traces:**
- Context propagation failure
- Different sampling decisions
- Export failures

**High costs:**
- Lower sampling rate
- Filter high-volume operations
- Use time-based sampling

## Integration Examples

### AWS SDK

Automatic instrumentation:

```go
// No code changes required
ec2Client.DescribeInstances(ctx, input)
// Span automatically created and exported
```

### Custom Operations

Manual instrumentation:

```go
ctx, span := tracer.Start(ctx, "custom.operation")
defer span.End()

span.SetAttributes(
    attribute.String("operation", "process"),
    attribute.Int("items", 42),
)

// Do work
if err != nil {
    span.RecordError(err)
    span.SetStatus(codes.Error, err.Error())
    return err
}

span.SetStatus(codes.Ok, "success")
```

## Related Documentation

- [Distributed Tracing How-To](../how-to/distributed-tracing.md)
- [Metrics Reference](./metrics.md)
- [OpenTelemetry Specification](https://opentelemetry.io/docs/specs/otel/)
