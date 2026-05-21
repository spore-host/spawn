# Network Streaming Guide

Spawn pipelines support real-time network streaming between stages, enabling high-throughput data transfer without intermediate S3 storage.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────┐
│                   Application Layer                          │
│  ZeroMQ Patterns: PUSH/PULL, PUB/SUB, DEALER/ROUTER        │
└────────────────────┬────────────────────────────────────────┘
                     │
┌────────────────────┴────────────────────────────────────────┐
│                   Transport Layer                            │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐               │
│  │   TCP    │   │   QUIC   │   │   RDMA   │               │
│  │          │   │          │   │   (EFA)  │               │
│  │ Reliable │   │ 0-RTT    │   │ 100Gbps  │               │
│  │ Ordered  │   │ TLS 1.3  │   │ Zero-copy│               │
│  └──────────┘   └──────────┘   └──────────┘               │
└────────────────────┬────────────────────────────────────────┘
                     │
┌────────────────────┴────────────────────────────────────────┐
│              Discovery & Coordination                        │
│  spored mesh + /etc/spawn/pipeline-peers.json              │
└─────────────────────────────────────────────────────────────┘
```

## Transport Selection

Spawn automatically selects the best transport based on network topology:

| Scenario | Transport | Throughput | Latency | Use Case |
|----------|-----------|------------|---------|----------|
| Same AZ + EFA | **RDMA** | 100+ Gbps | <1 µs | HPC, ML training |
| Same AZ | **TCP** | 10-25 Gbps | <1 ms | General pipelines |
| Same Region | **QUIC** | 5-10 Gbps | 1-5 ms | Multi-AZ pipelines |
| Cross-Region | **QUIC** | 1-5 Gbps | 10-100 ms | Global pipelines |

### Transport Properties

**TCP (Transmission Control Protocol)**
- ✅ Reliable, ordered delivery
- ✅ Universal compatibility
- ✅ Well-understood, battle-tested
- ❌ Head-of-line blocking
- ❌ Slow connection establishment
- **Use for:** General-purpose pipelines, short-lived connections

**QUIC (Quick UDP Internet Connections)**
- ✅ 0-RTT connection establishment
- ✅ Built-in TLS 1.3 encryption
- ✅ No head-of-line blocking
- ✅ Connection migration (survives IP changes)
- ❌ Requires UDP (some firewalls block)
- ❌ Higher CPU overhead than TCP
- **Use for:** Multi-region, high-latency networks, mobile/edge

**RDMA (Remote Direct Memory Access via EFA)**
- ✅ 100+ Gbps throughput
- ✅ Sub-microsecond latency
- ✅ Zero-copy, kernel bypass
- ✅ Minimal CPU usage
- ❌ Requires EFA-enabled instances (p4d, p5, c5n, etc.)
- ❌ Same placement group required
- ❌ More complex setup
- **Use for:** HPC, distributed ML training, large data transfers

## ZeroMQ Patterns

ZeroMQ provides high-level messaging patterns optimized for pipelines:

### PUSH/PULL - Linear Pipeline

```
producer (PUSH) → processor (PULL → PUSH) → consumer (PULL)
```

**Use case:** Sequential processing stages

**Example:**
```go
import "github.com/spore-host/spore-host/spawn/pkg/streaming"

// Producer
push, _ := streaming.NewZMQTransport(streaming.ZMQConfig{
    Pattern:  streaming.ZMQPush,
    Endpoint: "tcp://*:5555",
})
push.Bind()
push.Send([]byte("data"))

// Consumer
pull, _ := streaming.NewZMQTransport(streaming.ZMQConfig{
    Pattern:  streaming.ZMQPull,
    Endpoint: "tcp://upstream-host:5555",
})
pull.Connect()
data, _ := pull.Receive()
```

### PUB/SUB - Fan-Out Broadcasting

```
producer (PUB) ──┬→ consumer1 (SUB)
                 ├→ consumer2 (SUB)
                 └→ consumer3 (SUB)
```

**Use case:** Broadcast data to multiple downstream stages

**Example:**
```go
// Publisher
pub, _ := streaming.NewZMQTransport(streaming.ZMQConfig{
    Pattern:  streaming.ZMQPub,
    Endpoint: "tcp://*:5556",
})
pub.Bind()
pub.Send([]byte("broadcast data"))

// Subscriber
sub, _ := streaming.NewZMQTransport(streaming.ZMQConfig{
    Pattern:   streaming.ZMQSub,
    Endpoint:  "tcp://publisher-host:5556",
    Subscribe: "", // "" = all messages
})
sub.Connect()
data, _ := sub.Receive()
```

### DEALER/ROUTER - Load Balancing

```
producer1 (DEALER) ──┐
producer2 (DEALER) ──┼→ router (ROUTER) → workers
producer3 (DEALER) ──┘
```

**Use case:** Distribute work across multiple workers

## Pipeline Configuration

### Basic Streaming Pipeline

```json
{
  "pipeline_id": "streaming-example",
  "stages": [
    {
      "stage_id": "producer",
      "instance_type": "c5.xlarge",
      "instance_count": 1,
      "command": "python producer.py",
      "data_output": {
        "mode": "stream",
        "protocol": "zmq",
        "pattern": "push",
        "port": 5555
      }
    },
    {
      "stage_id": "consumer",
      "instance_type": "c5.xlarge",
      "instance_count": 1,
      "command": "python consumer.py",
      "depends_on": ["producer"],
      "data_input": {
        "mode": "stream",
        "source_stage": "producer",
        "protocol": "zmq",
        "pattern": "pull"
      }
    }
  ]
}
```

### High-Performance RDMA Pipeline

```json
{
  "pipeline_id": "hpc-rdma",
  "stages": [
    {
      "stage_id": "simulation",
      "instance_type": "c5n.18xlarge",
      "instance_count": 4,
      "placement_group": "auto",
      "efa_enabled": true,
      "command": "mpirun -np 4 --hostfile /etc/spawn/hostfile ./simulate",
      "data_output": {
        "mode": "stream",
        "protocol": "rdma",
        "port": 7500
      }
    },
    {
      "stage_id": "analysis",
      "instance_type": "c5n.9xlarge",
      "instance_count": 2,
      "placement_group": "auto",
      "efa_enabled": true,
      "command": "./analyze",
      "depends_on": ["simulation"],
      "data_input": {
        "mode": "stream",
        "source_stage": "simulation",
        "protocol": "rdma"
      }
    }
  ]
}
```

## Application Examples

### Python with ZeroMQ

```python
#!/usr/bin/env python3
import zmq
import json

# Load peer discovery
with open('/etc/spawn/pipeline-peers.json') as f:
    peers = json.load(f)

# Get upstream peer
upstream = peers['upstream_stages']['producer'][0]
upstream_ip = upstream['private_ip']

# Connect ZeroMQ PULL socket
context = zmq.Context()
socket = context.socket(zmq.PULL)
socket.connect(f"tcp://{upstream_ip}:5555")

# Receive and process
while True:
    data = socket.recv()
    process(data)
```

### Go with ZeroMQ

```go
package main

import (
    "github.com/spore-host/spore-host/spawn/pkg/streaming"
    "github.com/spore-host/spore-host/spawn/pkg/pipeline"
)

func main() {
    // Load peer discovery
    peers, _ := pipeline.LoadPeerDiscoveryFile("/etc/spawn/pipeline-peers.json")

    // Get upstream peer
    upstream := peers.GetFirstUpstreamPeer()

    // Create ZeroMQ transport
    pull, _ := streaming.NewZMQTransport(streaming.ZMQConfig{
        Pattern:  streaming.ZMQPull,
        Endpoint: fmt.Sprintf("tcp://%s:5555", upstream.PrivateIP),
    })
    pull.Connect()

    // Process data
    for {
        data, _ := pull.Receive()
        process(data)
    }
}
```

### C++ with RDMA (libfabric)

```cpp
#include <rdma/fabric.h>
#include <rdma/fi_endpoint.h>

// Initialize libfabric
struct fi_info *hints = fi_allocinfo();
hints->caps = FI_MSG;
hints->ep_attr->type = FI_EP_MSG;

struct fi_info *fi;
fi_getinfo(FI_VERSION(1, 9), "efa", NULL, 0, hints, &fi);

// Create endpoint
struct fid_fabric *fabric;
fi_fabric(fi->fabric_attr, &fabric, NULL);

struct fid_ep *ep;
fi_endpoint(domain, fi, &ep, NULL);

// Send data (zero-copy)
fi_send(ep, buffer, length, NULL, dest_addr, NULL);
```

## Performance Optimization

### Batching

Batch small messages to amortize protocol overhead:

```python
# Bad: Send one record at a time
for record in records:
    socket.send(record)  # 1000x overhead

# Good: Batch records
batch = b'\n'.join(records)
socket.send(batch)  # 1x overhead
```

### Zero-Copy

Use memory-mapped files or shared memory:

```python
import mmap
import zmq

# Producer: Memory-map file
with open('/dev/shm/data.bin', 'r+b') as f:
    mm = mmap.mmap(f.fileno(), 0)
    socket.send(mm, copy=False)  # Zero-copy send
```

### Compression

Compress data for network-bound workloads:

```python
import lz4.frame

# Compress before sending
compressed = lz4.frame.compress(data)
socket.send(compressed)

# Decompress after receiving
data = lz4.frame.decompress(compressed)
```

### Parallel Connections

Use multiple sockets for CPU-bound workloads:

```python
import multiprocessing

def worker(socket_id):
    context = zmq.Context()
    socket = context.socket(zmq.PULL)
    socket.connect(f"tcp://upstream:555{socket_id}")

    while True:
        data = socket.recv()
        process(data)

# Create 8 workers (one per CPU core)
for i in range(8):
    multiprocessing.Process(target=worker, args=(i,)).start()
```

## Benchmarks

### TCP Throughput

| Instance Type | Network | Single Thread | Multi-Thread |
|---------------|---------|---------------|--------------|
| c5.xlarge | 10 Gbps | 1.2 GB/s | N/A |
| c5.9xlarge | 10 Gbps | 1.2 GB/s | 9.5 GB/s (8 cores) |
| c5n.9xlarge | 50 Gbps | 6.0 GB/s | 48 GB/s |
| c5n.18xlarge | 100 Gbps | 6.0 GB/s | 96 GB/s |

### RDMA Throughput (EFA)

| Instance Type | Network | Single Thread | Multi-Thread | MPI |
|---------------|---------|---------------|--------------|-----|
| c5n.18xlarge | 100 Gbps | 12 GB/s | 95 GB/s | 100 GB/s |
| p4d.24xlarge | 400 Gbps | 40 GB/s | 380 GB/s | 400 GB/s |
| p5.48xlarge | 3200 Gbps | 300 GB/s | 3000 GB/s | 3200 GB/s |

### Latency

| Transport | Same AZ | Cross-AZ | Cross-Region |
|-----------|---------|----------|--------------|
| TCP | 0.1-0.5 ms | 1-2 ms | 20-100 ms |
| QUIC | 0.2-0.7 ms | 1-3 ms | 15-90 ms |
| RDMA | 1-5 µs | N/A | N/A |

## Troubleshooting

### Connection Refused

**Symptom:** `connect: connection refused`

**Causes:**
- Upstream stage not ready yet (instances still launching)
- Security group not allowing traffic
- Wrong port number

**Fix:**
```bash
# Check if upstream is listening
nc -zv upstream-host 5555

# Check security group rules
aws ec2 describe-security-groups --group-ids sg-xxx
```

### Slow Throughput

**Symptom:** Transfer rate much lower than expected

**Causes:**
- Small message size (protocol overhead)
- Not batching
- TCP send/receive buffers too small

**Fix:**
```python
# Increase ZeroMQ high water mark
socket.setsockopt(zmq.SNDHWM, 10000)
socket.setsockopt(zmq.RCVHWM, 10000)

# Increase OS buffers
import socket
sock.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 16*1024*1024)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 16*1024*1024)
```

### EFA Not Working

**Symptom:** RDMA transport falls back to TCP

**Causes:**
- Instance type doesn't support EFA
- Not in placement group
- EFA drivers not installed

**Fix:**
```bash
# Check if EFA device exists
ls /sys/class/infiniband/

# Check EFA driver version
modinfo efa

# Verify libfabric installed
fi_info -p efa
```

## Best Practices

1. **Use placement groups for EFA** - Required for RDMA performance
2. **Batch small messages** - Amortize protocol overhead
3. **Monitor network utilization** - `iftop`, `nethogs`, CloudWatch metrics
4. **Set appropriate timeouts** - Handle transient network issues
5. **Use ZeroMQ for flexibility** - Easy to change patterns without app changes
6. **Test at scale** - Network performance varies with instance count
7. **Log transport selection** - Debug which transport is being used

## See Also

- [Pipeline Definition Reference](pipeline-definition.md)
- [Example Streaming Applications](../examples/streaming/)
- [ZeroMQ Guide](http://zguide.zeromq.org/)
- [AWS EFA Documentation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa.html)
- [QUIC Protocol](https://www.chromium.org/quic/)
