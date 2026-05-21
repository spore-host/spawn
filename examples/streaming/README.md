# Network Streaming Examples

This directory contains example applications demonstrating Spawn's network streaming capabilities for real-time data transfer between pipeline stages.

## Overview

Network streaming mode allows pipeline stages to communicate directly via TCP or gRPC, enabling:

- **Real-time data processing** - No intermediate S3 uploads/downloads
- **Lower latency** - Direct connections between instances
- **Higher throughput** - Network bandwidth limited only by instance types
- **Streaming patterns** - Continuous data flow, not batch-based

## Example Pipeline

The `streaming-demo.json` pipeline demonstrates a three-stage streaming workflow:

```
producer → processor (×2) → consumer
```

1. **Producer** - Generates synthetic data and streams to processors
2. **Processor** - Receives, transforms, and forwards data to consumer
3. **Consumer** - Receives processed data and writes to file (uploaded to S3)

## Architecture

### Peer Discovery

Each instance automatically gets a peer discovery file at `/etc/spawn/pipeline-peers.json`:

```json
{
  "pipeline_id": "streaming-demo",
  "stage_id": "processor",
  "stage_index": 1,
  "instance_index": 0,
  "stage_peers": [
    {
      "stage_id": "processor",
      "instance_id": "i-abc123",
      "private_ip": "10.0.1.50",
      "dns_name": "processor-0.streaming-demo.spore.host"
    }
  ],
  "upstream_stages": {
    "producer": [
      {
        "stage_id": "producer",
        "instance_id": "i-def456",
        "private_ip": "10.0.1.40",
        "dns_name": "producer-0.streaming-demo.spore.host"
      }
    ]
  },
  "downstream_stages": {
    "consumer": [
      {
        "stage_id": "consumer",
        "instance_id": "i-ghi789",
        "private_ip": "10.0.1.60",
        "dns_name": "consumer-0.streaming-demo.spore.host"
      }
    ]
  }
}
```

### Network Protocol

These examples use a simple length-prefixed protocol:

```
[4-byte length][data][4-byte length][data]...
```

- **Length**: 32-bit unsigned integer, big-endian
- **Data**: Arbitrary binary data (text, JSON, protobuf, etc.)
- **Max chunk size**: 100 MB

This protocol is implemented in `spawn/pkg/streaming/tcp.go` for Go applications.

## Running the Example

### 1. Launch Pipeline

```bash
# Validate pipeline definition
spawn pipeline validate examples/pipelines/streaming-demo.json

# Launch pipeline
spawn pipeline launch examples/pipelines/streaming-demo.json --wait
```

### 2. Monitor Progress

```bash
# Check pipeline status
spawn pipeline status streaming-demo

# Watch in real-time
spawn pipeline status streaming-demo --watch

# View logs from specific stage
spawn ssh streaming-demo --stage producer --command "tail -f /var/log/spawn/stage.log"
```

### 3. Collect Results

```bash
# Download results when complete
spawn pipeline collect streaming-demo --output ./results/
```

## Example Output

**Producer Log:**
```
Producer listening on port 50000
Expecting 2 downstream consumers
Accepted connection 1/2 from 10.0.1.51:34567
Accepted connection 2/2 from 10.0.1.52:34568
All consumers connected. Streaming 100 batches...
Sent batch 10/100 (10000 records/sec)
Sent batch 20/100 (10500 records/sec)
...
Streaming complete. Closing connections...
Sent 100000 records in 9.5s (10526 records/sec)
```

**Processor Log:**
```
Processor configuration:
  Upstream: 10.0.1.40:50000
  Listening on port: 50001
  Expected downstream consumers: 1
Connecting to upstream producer at 10.0.1.40:50000
Connected to upstream producer
Waiting for 1 downstream connections...
Accepted connection 1/1 from 10.0.1.60:45678
Received 10 batches from upstream
Processed and forwarded 10 batches
...
Finished processing 100 batches
```

**Consumer Log:**
```
Consumer configuration:
  Upstream processors: 2
  Output file: /data/results.csv
Connecting to processor at 10.0.1.51:50001
Connected to processor
Received 10 batches, 10000 records (10234 records/sec)
...
Consumer finished:
  Total batches: 100
  Total records: 100000
  Duration: 9.8s
  Rate: 10204 records/sec
  Output: /data/results.csv
```

## Customization

### Change Data Format

Modify the `generate_batch()` and `process_batch()` functions to use your data format:

- CSV (current example)
- JSON
- Protocol Buffers
- Apache Arrow
- Custom binary format

### Fan-Out Pattern

Connect one producer to multiple consumers:

```python
# In producer.py
connections = []
for i in range(total_consumers):
    conn, addr = server_sock.accept()
    connections.append(conn)

# Broadcast to all
for conn in connections:
    send_chunk(conn, batch_data)
```

### Fan-In Pattern

Connect multiple producers to one consumer:

```python
# In consumer.py
import threading

def receive_from_upstream(upstream_ip, upstream_port, queue):
    # Connect and receive
    ...

# Start thread for each upstream
threads = []
for upstream in upstream_instances:
    t = threading.Thread(target=receive_from_upstream,
                        args=(upstream['private_ip'], 50000, queue))
    t.start()
    threads.append(t)
```

## Performance Tips

### Network Optimization

1. **Use enhanced networking** - Instance types with ENA (c5, m5, p3, etc.)
2. **Use placement groups** - For minimum latency within same AZ
3. **Enable EFA** - For multi-instance MPI workloads (p4d, p5, c5n)
4. **Tune TCP buffers**:
   ```bash
   sysctl -w net.core.rmem_max=134217728
   sysctl -w net.core.wmem_max=134217728
   ```

### Application Optimization

1. **Batch size** - Larger batches reduce protocol overhead
2. **Compression** - Use zlib/lz4 for compressible data
3. **Multiple connections** - One per CPU core for parallel processing
4. **Zero-copy** - Use `sendfile()` for file transfers

### Expected Throughput

| Instance Type | Network | Typical Throughput |
|---------------|---------|-------------------|
| c5.xlarge     | 10 Gbps | 1.0 GB/s |
| c5n.18xlarge  | 100 Gbps| 10 GB/s |
| p3.8xlarge    | 10 Gbps | 1.0 GB/s |
| p4d.24xlarge  | 400 Gbps (EFA) | 30-40 GB/s |

## Alternative: gRPC

For structured RPC-style communication, consider gRPC:

```python
# producer.proto
service DataStream {
  rpc StreamData(stream DataBatch) returns (stream Ack);
}

message DataBatch {
  repeated Record records = 1;
}
```

Benefits:
- Built-in retries and error handling
- Bidirectional streaming
- Schema validation (Protobuf)
- HTTP/2 multiplexing

See `spawn/pkg/streaming/grpc.go` for Go gRPC helpers.

## Debugging

### Check Connectivity

```bash
# SSH into processor instance
spawn ssh streaming-demo --stage processor

# Check peer discovery file
cat /etc/spawn/pipeline-peers.json

# Test TCP connection to upstream
nc -zv 10.0.1.40 50000
```

### Monitor Network Traffic

```bash
# Install on instance
sudo yum install -y tcpdump

# Capture traffic
sudo tcpdump -i eth0 port 50000 -w /tmp/capture.pcap

# Download and analyze
spawn scp streaming-demo:/tmp/capture.pcap ./capture.pcap
wireshark capture.pcap
```

### Common Issues

**Connection refused**: Upstream stage not ready yet. Pipeline orchestrator ensures dependencies are met, but there can be a few seconds of startup time.

**Broken pipe**: Downstream disconnected. Check consumer logs for errors.

**Slow throughput**: Check network bandwidth, TCP buffer sizes, and batch sizes.

## See Also

- [Pipeline Definition Reference](../../docs/pipeline-definition.md)
- [Streaming Mode Guide](../../docs/streaming.md)
- [TCP Helpers](../../pkg/streaming/tcp.go)
- [Peer Discovery](../../pkg/pipeline/peer_discovery.go)
