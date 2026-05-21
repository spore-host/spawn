# How-To Guides

Task-oriented guides for specific scenarios. Unlike tutorials, these are focused recipes that assume you have basic spawn knowledge.

## Launching Instances

### [Launch Instances](launch-instances.md)
Quick recipes for common instance launching scenarios:
- Development, production, GPU instances
- Custom configurations (network, storage, lifecycle)
- IAM configuration
- Spot instances
- User data scripts
- Complete examples (web server, ML training, batch processing)

---

## Batch Processing

### [Parameter Sweeps](parameter-sweeps.md)
Advanced parameter sweep patterns:
- Grid search strategies
- Random search
- Cartesian product generation
- Multi-region sweeps
- Resume failed sweeps
- Result aggregation

### [Job Arrays](job-arrays.md)
Advanced job array patterns:
- Chunked processing
- Dynamic task distribution with SQS
- Fault tolerance and retries
- Monitoring large arrays
- Cost optimization

### [Batch Queues](batch-queues.md)
Advanced queue patterns:
- Complex DAG workflows
- Conditional execution
- Parallel job stages
- Error handling strategies
- Result validation

---

## Cost Optimization

### [Spot Instances](spot-instances.md)
Complete guide to using spot instances:
- When to use spot
- Setting max price
- Handling interruptions
- Spot vs on-demand comparison
- Spot instance pools

### [Cost Optimization](cost-optimization.md)
Advanced cost-saving techniques:
- Right-sizing methodology
- Reserved instances (when spawn isn't the answer)
- Savings plans
- Multi-region cost arbitrage
- Automated cost reporting

---

## Networking & Connectivity

### [SSH & Connectivity](ssh-connectivity.md)
Advanced SSH configuration:
- Custom SSH keys
- SSH config file setup
- Bastion hosts
- Port forwarding
- Session Manager
- VPN integration

### [Custom Networking](custom-networking.md)
VPC and networking configuration:
- Custom VPCs
- Private subnets
- NAT gateways
- VPC peering
- Security group management
- Network ACLs

---

## AMIs & Images

### [Custom AMIs](custom-amis.md)
Create and manage custom AMIs:
- Building custom AMIs
- Pre-installing software
- AMI sharing and permissions
- AMI lifecycle management
- Golden image patterns

---

## Instance Selection

### [Instance Selection](instance-selection.md)
Choose the right instance type:
- CPU-optimized workloads
- Memory-optimized workloads
- GPU selection guide
- ARM vs x86
- Performance testing methodology
- Cost/performance analysis

---

## Security & IAM

### [Security & IAM](security-iam.md)
Security best practices:
- IAM role configuration
- Least privilege policies
- Secrets management
- Network security
- Compliance considerations
- Audit logging

---

## Monitoring & Debugging

### [Debugging Failed Instances](debugging.md)
Troubleshoot common issues:
- Reading cloud-init logs
- SSH connection failures
- Out of memory errors
- CUDA errors
- Timeout issues
- Permission errors

### [Monitoring at Scale](monitoring-scale.md)
Monitor large deployments:
- CloudWatch dashboards
- Custom metrics
- Log aggregation
- Alert escalation
- Cost anomaly detection

---

## Integration

### [CI/CD Integration](cicd-integration.md)
Use spawn in CI/CD pipelines:
- GitHub Actions
- GitLab CI
- Jenkins
- Automated testing
- Build farms

### [Slurm Integration](slurm-integration.md)
Integrate with Slurm workload manager:
- Convert Slurm scripts
- Job dependencies
- Array jobs
- Resource requests

---

## Advanced Topics

### [Multi-Account Setup](multi-account.md)
Manage spawn across AWS accounts:
- AWS Organizations
- Cross-account IAM roles
- Centralized billing
- Account isolation

### [Disaster Recovery](disaster-recovery.md)
Backup and recovery strategies:
- Data backup
- AMI snapshots
- Cross-region replication
- Recovery procedures

---

## Quick Links by Use Case

**I want to...** → **See this guide:**

- Launch a specific type of instance → [Launch Instances](launch-instances.md)
- Save money with spot instances → [Spot Instances](spot-instances.md)
- Run parameter sweeps efficiently → [Parameter Sweeps](parameter-sweeps.md)
- Process thousands of files → [Job Arrays](job-arrays.md)
- Build a multi-step pipeline → [Batch Queues](batch-queues.md)
- Reduce my AWS bill → [Cost Optimization](cost-optimization.md)
- Fix SSH connection issues → [SSH & Connectivity](ssh-connectivity.md)
- Choose the right instance type → [Instance Selection](instance-selection.md)
- Create custom AMIs → [Custom AMIs](custom-amis.md)
- Secure my instances → [Security & IAM](security-iam.md)
- Debug failed instances → [Debugging Failed Instances](debugging.md)
- Use spawn in CI/CD → [CI/CD Integration](cicd-integration.md)

---

## Format

Each how-to guide follows this structure:

1. **Problem statement** - What you want to achieve
2. **Solution overview** - High-level approach
3. **Step-by-step instructions** - Concrete steps with commands
4. **Examples** - Real-world scenarios
5. **Troubleshooting** - Common issues
6. **Related resources** - Links to tutorials, references

---

## Contributing

Found an issue or have a suggestion? [Open an issue](https://github.com/spore-host/spore-host/issues/new?labels=type:docs,component:spawn)
