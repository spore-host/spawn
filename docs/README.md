# spawn Documentation

Complete documentation for spawn - Ephemeral AWS EC2 instance launcher.

## Documentation Structure

This documentation follows the **[Diátaxis](https://diataxis.fr/)** framework, organizing content by user needs:

```
                Learning-oriented   |   Task-oriented
                    (acquisition)   |   (application)
            ──────────────────────────────────────────
                    TUTORIALS       |     HOW-TO GUIDES
            ──────────────────────────────────────────
                                    |
            ──────────────────────────────────────────
                  EXPLANATION       |     REFERENCE
            ──────────────────────────────────────────
              Understanding-oriented | Information-oriented
                  (cognition)       |   (description)
```

## Quick Navigation

### 🎓 [Tutorials](tutorials/)
**Learning-oriented** - Step-by-step lessons for beginners

- [Getting Started](tutorials/01-getting-started.md) (15 min)
- [Your First Instance](tutorials/02-first-instance.md) (20 min)
- [Parameter Sweeps](tutorials/03-parameter-sweeps.md) (30 min)
- [Job Arrays](tutorials/04-job-arrays.md) (30 min)
- [Batch Queues](tutorials/05-batch-queues.md) (45 min)
- [Cost Management](tutorials/06-cost-management.md) (20 min)
- [Monitoring & Alerts](tutorials/07-monitoring-alerts.md) (30 min)

### 📖 [How-To Guides](how-to/)
**Task-oriented** - Solutions for specific tasks

- [Launch Instances](how-to/launch-instances.md)
- [Parameter Sweeps](how-to/parameter-sweeps.md)
- [Job Arrays](how-to/job-arrays.md)
- [Batch Queues](how-to/batch-queues.md)
- [Spot Instances](how-to/spot-instances.md)
- [Cost Optimization](how-to/cost-optimization.md)
- [Security & IAM](how-to/security-iam.md)
- [SSH Connectivity](how-to/ssh-connectivity.md)
- [Custom AMIs](how-to/custom-amis.md)
- [Custom Networking](how-to/custom-networking.md)
- [Instance Selection](how-to/instance-selection.md)
- [Debugging](how-to/debugging.md)
- [Monitoring at Scale](how-to/monitoring-scale.md)
- [CI/CD Integration](how-to/cicd-integration.md)
- [Slurm Integration](how-to/slurm-integration.md)
- [Multi-Account Setup](how-to/multi-account.md)
- [Disaster Recovery](how-to/disaster-recovery.md)

### 📚 [Reference](reference/)
**Information-oriented** - Complete technical reference

- **[Command Reference](reference/README.md)** - All commands and flags
  - [launch](reference/commands/launch.md) - Launch EC2 instances
  - [list](reference/commands/list.md) - List instances
  - More commands coming soon...
- **[Configuration](reference/configuration.md)** - Config file format
- **[Environment Variables](reference/environment-variables.md)** - All environment variables
- **[Exit Codes](reference/exit-codes.md)** - Command exit codes
- **[Parameter Files](reference/parameter-files.md)** - Parameter sweep format (Coming Soon)
- **[Queue Configs](reference/queue-configs.md)** - Batch queue format (Coming Soon)
- **[IAM Policies](reference/iam-policies.md)** - IAM permissions (Coming Soon)

### 💡 [Explanation](explanation/) (Coming Soon)
**Understanding-oriented** - Concepts and background

- Architecture
- Core Concepts
- Parameter Sweeps
- Job Arrays
- Batch Queues
- Cost Optimization
- Security
- Networking
- Storage
- Compliance

### 🏗️ [Architecture](architecture/) (Coming Soon)
**System design** - How spawn works internally

- Overview
- AWS Resources
- Multi-Account Setup
- Security Model
- Data Flow

### 🔧 [Troubleshooting](troubleshooting/) (Coming Soon)
**Problem-solving** - Common issues and fixes

- Common Errors
- Debugging Guide
- Performance Issues
- Connectivity Problems

## Getting Started

### New Users
1. Start with the main **[README](../README.md)** for overview and quick start
2. Follow **[Getting Started Tutorial](tutorials/01-getting-started.md)**
3. Browse **[How-To Guides](how-to/)** for specific tasks

### Experienced Users
- Jump to **[Command Reference](reference/README.md)** for detailed flag documentation
- Check **[How-To Guides](how-to/)** for specific workflows
- Read **[Explanation](explanation/)** for deeper understanding (Coming Soon)

### Developers
- Read **[Architecture](architecture/)** docs (Coming Soon)
- See **[CONTRIBUTING.md](../CONTRIBUTING.md)** for development setup (Coming Soon)
- Check **[API Documentation](../godoc/)** for Go package docs

## Documentation Status

### ✅ Phase 1: Essential Reference (Complete)
- [x] Command reference index
- [x] Configuration reference
- [x] Environment variables reference
- [x] Exit codes reference
- [x] All command pages (24 commands)
- [x] Parameter files reference
- [x] Queue configs reference
- [x] IAM policies reference

### ✅ Phase 2: Getting Started (Complete)
- [x] Tutorial 1: Getting Started
- [x] Tutorial 2: Your First Instance
- [x] Tutorial 3: Parameter Sweeps
- [x] Tutorial 4: Job Arrays
- [x] Tutorial 5: Batch Queues
- [x] Tutorial 6: Cost Management
- [x] Tutorial 7: Monitoring & Alerts
- [x] How-to: Launch Instances
- [x] How-to: Parameter Sweeps
- [x] How-to: Job Arrays
- [x] How-to: Batch Queues
- [x] How-to: Spot Instances
- [x] How-to: Cost Optimization
- [x] How-to: SSH Connectivity
- [x] How-to: Instance Selection
- [x] How-to: Custom AMIs
- [x] How-to: Security & IAM
- [x] How-to: Custom Networking
- [x] How-to: Debugging
- [x] How-to: Monitoring at Scale
- [x] How-to: CI/CD Integration
- [x] How-to: Slurm Integration
- [x] How-to: Multi-Account Setup
- [x] How-to: Disaster Recovery

### ⏳ Phase 3: Explanation & Architecture (Planned)
- [ ] Explanation: Architecture Overview
- [ ] Explanation: Core Concepts (TTL, idle detection, spot)
- [ ] Explanation: Security Model
- [ ] Explanation: Cost Optimization Theory
- [ ] Explanation: Multi-Account Architecture
- [ ] Architecture: System Design
- [ ] Architecture: AWS Resources
- [ ] Architecture: Data Flow
- [ ] Architecture: DNS System
- [ ] Architecture: Agent (spored) Design

### ⏳ Phase 4: Polish & Finalize (Planned)
- [ ] Complete FAQ
- [ ] Migration guides (Slurm, AWS Batch, etc.)
- [ ] CONTRIBUTING.md
- [ ] Architecture diagrams
- [ ] Video tutorials (optional)
- [ ] Troubleshooting guide expansion

## Documentation Conventions

### Notation
- `<required>` - Required argument
- `[optional]` - Optional argument
- `--flag` - Command flag
- `string|int|duration|bool` - Argument types
- `...` - Repeatable argument

### Duration Format
Durations use Go's time format:
- `30m` - 30 minutes
- `2h` - 2 hours
- `1d` - 1 day (24 hours)
- `3h30m` - 3 hours 30 minutes
- `1d12h` - 1 day 12 hours

### Code Examples
Examples use bash unless otherwise noted. Commands are designed to be copy-pasteable.

### Cross-References
- Internal links use relative paths
- External links open in new window
- Command references link to [reference/commands/](reference/commands/)

## Contributing to Documentation

Documentation contributions are welcome! See [CONTRIBUTING.md](../CONTRIBUTING.md) for guidelines. (Coming Soon)

**Documentation Style Guide:**
- Use active voice ("Launch an instance" not "An instance is launched")
- Keep examples realistic and runnable
- Include both simple and complex examples
- Explain the "why" not just the "what"
- Link to related documentation

## External Resources

### spawn Project
- [Main README](../README.md) - Overview and quick start
- [CHANGELOG](../CHANGELOG.md) - Version history
- [TROUBLESHOOTING](../TROUBLESHOOTING.md) - Common issues
- [IAM_PERMISSIONS](../IAM_PERMISSIONS.md) - Required AWS permissions

### Related Tools
- [truffle](../../truffle/README.md) - Instance discovery and quota management

### AWS Documentation
- [EC2 User Guide](https://docs.aws.amazon.com/ec2/)
- [Instance Types](https://aws.amazon.com/ec2/instance-types/)
- [Spot Instances](https://aws.amazon.com/ec2/spot/)
- [IAM Roles](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles.html)

## Documentation Feedback

Found an issue with the documentation? Please [open an issue](https://github.com/spore-host/spore-host/issues/new?labels=type:docs,component:spawn).

**Common feedback areas:**
- Unclear explanations
- Missing examples
- Broken links
- Outdated information
- Typos or grammar issues

## Search and Navigation

### Finding Commands
Use the [Command Reference Index](reference/README.md) to browse all available commands.

### Finding Topics
Browse by documentation type:
- **Learning something new?** → [Tutorials](tutorials/)
- **Solving a specific problem?** → [How-To Guides](how-to/)
- **Looking up syntax?** → [Reference](reference/)
- **Understanding how it works?** → [Explanation](explanation/)

### Searching Documentation
```bash
# Search all documentation
grep -r "parameter sweep" docs/

# Search command reference
grep -r "ttl" docs/reference/commands/

# Search specific topic
grep -r "cost" docs/how-to/
```

## Version Information

This documentation is for **spawn v0.13.1**.

For documentation for other versions:
- Latest: You're reading it!
- Previous versions: Check the [CHANGELOG](../CHANGELOG.md) and git tags

## License

Documentation is licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
