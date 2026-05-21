# Spawn - Future Enhancements

This document tracks potential future enhancements for the spawn CLI tool.

---

## Job Array Enhancements (Post-MVP)

### 1. Auto-Scaling Job Arrays
**Description:** Add instances to a running job array dynamically.

**Use Case:** Start with 4 instances, scale up to 8 as workload increases.

**CLI:**
```bash
# Launch initial array
spawn launch --count 4 --job-array-name compute --instance-type m7i.large

# Scale up later
spawn array scale compute --count 8  # Adds 4 more instances
```

**Implementation Considerations:**
- New instances get next available indices (4, 5, 6, 7)
- Peer discovery updates automatically
- Group DNS updates to include new IPs
- Existing instances notified of new peers (update peer file)

---

### 2. Cross-Region Job Arrays
**Description:** Launch job arrays that span multiple AWS regions.

**Use Case:** Distributed computing across geographic regions for latency-sensitive workloads.

**CLI:**
```bash
spawn launch --count 8 --job-array-name global-compute \
  --regions us-east-1,eu-west-1,ap-southeast-1 \
  --distribute-evenly  # 3+3+2 distribution
```

**Implementation Considerations:**
- Latency-based DNS routing for group DNS
- Peer list includes region information
- Network considerations (cross-region bandwidth costs)
- May need VPC peering or Transit Gateway

---

### 3. MPI/SLURM Integration
**Description:** Generate MPI hostfiles or SLURM configuration automatically.

**Use Case:** HPC workloads that use MPI for parallel computing.

**CLI:**
```bash
spawn launch --count 16 --job-array-name hpc-cluster \
  --mpi-hostfile /shared/hostfile \
  --slots-per-host 8
```

**Generated Files:**
- `/shared/hostfile` (MPI format):
  ```
  compute-0.1s69p4h.spore.host slots=8
  compute-1.1s69p4h.spore.host slots=8
  ...
  ```
- `/etc/slurm/slurm.conf` (SLURM format)

**Implementation Considerations:**
- Need shared filesystem (EFS) for hostfile
- Support different MPI implementations (OpenMPI, MPICH)
- Consider infiniband/enhanced networking

---

### 4. Failure Policies
**Description:** Configurable behavior when instances in array fail.

**Use Case:** Scientific computing where all instances must succeed, or fail together.

**CLI:**
```bash
# Terminate all if any instance fails
spawn launch --count 8 --job-array-name experiment \
  --failure-policy terminate-all

# Continue with remaining instances
spawn launch --count 8 --job-array-name resilient \
  --failure-policy continue
```

**Policies:**
- `terminate-all` - If any instance fails/terminates, kill entire array
- `continue` - Continue with remaining instances (default)
- `restart` - Auto-restart failed instances
- `quorum:N` - Terminate all if fewer than N instances remain

**Implementation Considerations:**
- Monitoring via CloudWatch or spored agent
- Spot interruption handling
- Health check mechanisms

---

### 5. Cost Allocation Tags
**Description:** Shared billing tags for job arrays to track costs per project.

**Use Case:** Track costs for specific experiments or projects across multiple instances.

**CLI:**
```bash
spawn launch --count 8 --job-array-name ml-training \
  --cost-center ML-Team \
  --project ImageNet2024 \
  --budget-alert 100  # Alert if costs exceed $100
```

**Features:**
- Automatic cost tracking by job array ID
- Budget alerts via CloudWatch
- Cost reports: `spawn array costs --job-array-name ml-training`
- Integration with AWS Cost Explorer tags

**Implementation Considerations:**
- Add tags: `spawn:cost-center`, `spawn:project`, `spawn:budget`
- CloudWatch billing alarms
- Cost reporting in `spawn list`

---

### 6. Job Array Templates
**Description:** Save and reuse job array configurations.

**Use Case:** Frequently launch same type of array (e.g., nightly ML training).

**CLI:**
```bash
# Save template
spawn launch --count 8 --instance-type m7i.large \
  --spot --ttl 8h --save-template ml-training

# Launch from template
spawn launch --template ml-training

# List templates
spawn template list

# Edit template
spawn template edit ml-training

# Delete template
spawn template delete ml-training
```

**Template Format (YAML):**
```yaml
name: ml-training
count: 8
instance_type: m7i.large
spot: true
ttl: 8h
user_data: |
  #!/bin/bash
  git clone https://github.com/org/ml-project
  cd ml-project
  python train.py --rank $JOB_ARRAY_INDEX
tags:
  project: ImageNet2024
  cost-center: ML-Team
```

**Storage:**
- Local: `~/.spawn/templates/`
- Shared: S3 bucket for team templates

---

### 7. Instance Health Checks
**Description:** Automated health monitoring and instance replacement.

**CLI:**
```bash
spawn launch --count 8 --job-array-name cluster \
  --health-check-url http://localhost:8080/health \
  --health-check-interval 60s \
  --auto-replace-unhealthy
```

**Features:**
- Periodic health checks via HTTP endpoint or custom script
- Auto-replace unhealthy instances
- Notification on failures
- Health status in `spawn list`

---

### 8. Dependency Management
**Description:** Control launch order and dependencies between instances.

**Use Case:** Launch leader node first, then workers that depend on leader.

**CLI:**
```bash
spawn launch --count 1 --job-array-name cluster --role leader

spawn launch --count 7 --job-array-name cluster --role worker \
  --wait-for-role leader  # Wait for leader to be ready
```

**Features:**
- Roles: leader, worker, coordinator, etc.
- Dependency graph: workers depend on leader
- Wait conditions: port open, file exists, health check passes

---

### 9. Ephemeral Storage Optimization
**Description:** Automatic EBS volume management for job arrays.

**CLI:**
```bash
spawn launch --count 8 --job-array-name data-processing \
  --ephemeral-storage 500GB \
  --storage-type gp3 \
  --storage-iops 10000
```

**Features:**
- Attach additional EBS volumes automatically
- Optimized IOPS/throughput for workload
- Auto-delete volumes when instances terminate
- Shared EFS for inter-instance data

---

### 10. GPU Job Arrays
**Description:** Enhanced support for GPU-accelerated job arrays.

**CLI:**
```bash
spawn launch --count 4 --job-array-name gpu-training \
  --instance-type p4d.24xlarge \
  --gpu-count 8 \
  --cuda-version 12.1 \
  --install-pytorch
```

**Features:**
- GPU driver installation
- CUDA toolkit setup
- Multi-GPU instance support
- Distributed training frameworks (PyTorch DDP, Horovod)
- GPU utilization monitoring

---

### 11. Spot Instance Retry Logic
**Description:** Automatically retry Spot instances in different AZs on failure.

**CLI:**
```bash
spawn launch --count 8 --job-array-name resilient \
  --spot \
  --spot-retry-zones us-east-1a,us-east-1b,us-east-1c \
  --spot-max-retries 3
```

**Features:**
- Try multiple AZs for Spot capacity
- Fallback to On-Demand if Spot unavailable
- Retry with exponential backoff

---

### 12. Job Array Checkpointing
**Description:** Save/restore job array state for long-running workloads.

**CLI:**
```bash
spawn array checkpoint save --job-array-name training \
  --checkpoint-dir s3://bucket/checkpoints

spawn array checkpoint restore --job-array-name training \
  --checkpoint-dir s3://bucket/checkpoints \
  --checkpoint-id 20240115-123456
```

**Features:**
- Periodic state snapshots to S3
- Restore from checkpoint on failure
- Resume workloads after Spot interruption

---

## General Spawn Enhancements

### 13. AMI Creation from Instance
**Description:** Create reusable AMIs from configured spawn instances.

**Use Case:** Configure an instance with software/data, then create an AMI to launch identical instances faster.

**CLI:**
```bash
# Launch and configure instance
spawn launch --instance-type m7i.large --name base-image
# ... SSH in, install software, configure ...

# Create AMI from running instance
spawn image create --instance-id i-abc123 --image-name ml-training-base \
  --description "ML training environment with PyTorch 2.1"

# Launch from custom AMI
spawn launch --instance-type m7i.large --ami ami-xyz789

# List custom AMIs
spawn image list

# Share AMI with another account
spawn image share --ami ami-xyz789 --account-id 123456789012

# Delete AMI (deregister + delete snapshots)
spawn image delete --ami ami-xyz789
```

**Features:**
- Automatic reboot handling (optional --no-reboot)
- Tag AMIs with spawn metadata
- Track AMI ownership and creation time
- Clean up old AMIs automatically (retention policy)
- Cost tracking for stored snapshots

**Implementation Considerations:**
- Use EC2 CreateImage API
- Handle running vs stopped instances
- Snapshot EBS volumes
- Tag AMIs with `spawn:created-by`, `spawn:source-instance`, etc.
- Store AMI metadata in DynamoDB or tags

---

### 14. Job Completion Signal (Sentinel)
**Description:** Flexible mechanism for instances to signal completion and trigger actions.

**Use Case:** Long-running compute jobs that should terminate/stop the instance when done, without manual intervention.

**CLI:**
```bash
# Launch with completion monitoring
spawn launch --instance-type m7i.large \
  --on-complete terminate \
  --completion-file /tmp/JOB_DONE \
  --completion-delay 2m

# Or use completion command (runs on instance)
spawn launch --instance-type m7i.large \
  --on-complete terminate \
  --completion-command "test -f /output/results.csv"

# Or use HTTP endpoint check
spawn launch --instance-type m7i.large \
  --on-complete stop \
  --completion-url "http://localhost:8080/status" \
  --completion-check-interval 60s
```

**Completion Triggers:**
- **File-based** (current): Touch `/tmp/SPAWN_COMPLETE` when done
- **Command-based**: Exit code 0 = complete, non-zero = continue
- **HTTP-based**: HTTP 200 response = complete
- **Process-based**: Specific process exits
- **S3-based**: Specific S3 object exists

**Actions on Completion:**
- `terminate` - Terminate instance
- `stop` - Stop instance (preserve for later)
- `hibernate` - Hibernate instance
- `snapshot` - Create AMI, then terminate
- `notify` - Send SNS/email, keep running

**Advanced Features:**
```bash
# Multiple completion criteria (AND logic)
spawn launch --on-complete terminate \
  --completion-file /tmp/DONE \
  --completion-command "grep 'SUCCESS' /var/log/app.log"

# Retry on failure
spawn launch --on-complete terminate \
  --completion-retry 3 \
  --completion-retry-delay 5m

# Timeout fallback
spawn launch --on-complete terminate \
  --completion-timeout 8h \
  --on-timeout stop  # If not complete after 8h, stop instead
```

**User Scripts Can Signal Completion:**
```bash
#!/bin/bash
# User's compute script
python train_model.py

# Signal completion
touch /tmp/SPAWN_COMPLETE
echo "Job complete, instance will terminate in 2 minutes"
```

**Or via API:**
```bash
# Direct API call
curl -X POST http://localhost:9999/api/complete
```

**Implementation Considerations:**
- Spored agent monitors completion conditions
- Configurable check interval (default: 60s)
- Grace period before action (default: 30s)
- Logging of completion events to CloudWatch
- Notification options (SNS, email, webhook)
- Prevent false positives (file briefly exists, then deleted)

**Integration with Job Arrays:**
```bash
# Per-instance completion
spawn launch --count 8 --job-array-name train \
  --on-complete terminate \
  --completion-file /tmp/DONE

# Array-wide completion (all must complete)
spawn launch --count 8 --job-array-name train \
  --on-complete terminate \
  --completion-mode array \
  --completion-file /tmp/DONE
  # Terminates all instances once ALL have touched /tmp/DONE
```

**Completion Modes for Job Arrays:**
- `individual` (default) - Each instance completes independently
- `array` - All instances must complete before any action
- `leader` - Only leader (index 0) completion matters
- `quorum:N` - At least N instances must complete

---

### 15. Web Dashboard (Already Planned)
**Description:** Web UI for viewing and managing spawn instances.

**Features:**
- View all instances across regions
- Start/stop/terminate instances
- Real-time metrics and logs
- Cost tracking

**Status:** Design complete, implementation in progress.

---

### 14. Session Manager Integration
**Description:** SSH via AWS Systems Manager Session Manager.

**CLI:**
```bash
spawn connect my-instance --via-ssm
```

**Benefits:**
- No need for public IPs
- Works in private subnets
- Audit logging in CloudTrail
- No SSH key management

---

### 15. CloudWatch Integration
**Description:** Built-in metrics and alarms.

**Features:**
- CPU/memory/disk monitoring
- Cost tracking per instance
- TTL countdown alerts
- Custom CloudWatch dashboards

---

### 16. Notebook Integration
**Description:** Launch Jupyter/VSCode notebooks automatically.

**CLI:**
```bash
spawn launch --instance-type m7i.large --notebook jupyter
# Returns: https://my-instance.spore.host:8888?token=abc123
```

---

### 17. Docker Support
**Description:** Run Docker containers on spawned instances.

**CLI:**
```bash
spawn launch --instance-type m7i.large \
  --docker-image pytorch/pytorch:latest \
  --docker-command "python train.py"
```

---

### 18. S3 Data Transfer Optimization
**Description:** Automatic S3 data sync to instances.

**CLI:**
```bash
spawn launch --instance-type m7i.large \
  --sync-s3 s3://my-bucket/dataset /data/dataset \
  --sync-on-launch
```

---

## Contributing

To propose a new enhancement:
1. Add it to this file with description, use case, and implementation considerations
2. Open a GitHub issue for discussion
3. Mark status as "proposed" until approved

**Enhancement Status:**
- 🔵 Proposed - Idea stage
- 🟢 Approved - Ready for implementation
- 🟡 In Progress - Being implemented
- ✅ Complete - Shipped

---

## Prioritization Criteria

When evaluating enhancements, consider:
1. **User Impact** - How many users benefit?
2. **Complexity** - Implementation effort and maintenance burden
3. **Dependencies** - Does it require external services?
4. **Cost** - AWS costs for users
5. **Maintenance** - Long-term support requirements

**High Priority:**
- Job array enhancements (1-6) - Core functionality
- Session Manager integration - Security improvement
- CloudWatch integration - Observability

**Medium Priority:**
- MPI/SLURM integration - Specific use case (HPC)
- GPU enhancements - Growing demand
- Notebook integration - Developer productivity

**Low Priority:**
- Advanced checkpointing - Complex, niche use case
- Docker support - Many alternatives exist
