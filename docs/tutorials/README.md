# spawn Tutorials

Step-by-step guides to learn spawn from beginner to advanced. Follow in order for best results.

## Getting Started (Beginners)

### [Tutorial 1: Getting Started](01-getting-started.md)
**Duration:** 15 minutes | **Level:** Beginner

Your first steps with spawn:
- Install spawn
- Configure AWS credentials
- Launch your first instance
- Connect via SSH
- Terminate instances

**Prerequisites:** AWS account, basic command line knowledge

---

### [Tutorial 2: Your First Instance](02-first-instance.md)
**Duration:** 20 minutes | **Level:** Beginner

Deep dive into instance configuration:
- Choose the right instance type
- Understand AMIs
- Configure security groups and SSH keys
- Manage instance lifecycle (stop, start, hibernate)
- Use tags for organization

**Prerequisites:** [Tutorial 1: Getting Started](01-getting-started.md)

---

## Intermediate (Parallel & Batch Processing)

### [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md)
**Duration:** 30 minutes | **Level:** Intermediate

Launch multiple instances with different configurations:
- Create parameter sweep files (YAML/JSON)
- Launch dozens of instances simultaneously
- Monitor sweep progress
- Collect results
- ML hyperparameter tuning workflows
- Batch data processing

**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

---

### [Tutorial 4: Job Arrays](04-job-arrays.md)
**Duration:** 30 minutes | **Level:** Intermediate

Launch hundreds of identical instances in parallel:
- Launch job arrays (100s of instances)
- Use array indices for task distribution
- Process data files in parallel
- Monte Carlo simulations
- Cost optimization strategies

**Prerequisites:** [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md)

---

### [Tutorial 5: Batch Queues](05-batch-queues.md)
**Duration:** 45 minutes | **Level:** Intermediate

Run sequential jobs with dependencies:
- Create queue configuration files
- Define job dependencies (DAGs)
- Set retry strategies and timeouts
- Monitor queue execution
- Build ML training pipelines
- ETL workflows

**Prerequisites:** [Tutorial 4: Job Arrays](04-job-arrays.md)

---

## Advanced (Operations & Cost Management)

### [Tutorial 6: Cost Management](06-cost-management.md)
**Duration:** 20 minutes | **Level:** Intermediate

Track and optimize AWS costs:
- Track costs for instances and sweeps
- Set budget alerts
- Analyze spending patterns
- Optimize costs with spot instances
- Right-size instance types
- Avoid unexpected bills

**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

---

### [Tutorial 7: Monitoring & Alerts](07-monitoring-alerts.md)
**Duration:** 30 minutes | **Level:** Intermediate

Set up monitoring and notifications:
- Monitor instance status and health
- Set up Slack/Discord/Email alerts
- Create cost threshold alerts
- Monitor parameter sweeps
- Debug failed instances

**Prerequisites:** [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md)

---

## Cost-Optimized Workflows (Intermediate–Advanced)

### [Tutorial 8: Finding EC2 Capacity Before You Launch](08-finding-ec2-capacity.md)
**Duration:** 20 minutes | **Level:** Intermediate

Pre-flight capacity and cost checks with `truffle`:
- Check vCPU service quotas
- Compare spot prices across instance families (Intel, AMD, Graviton)
- Filter to AZs with active capacity
- Extract the cheapest region programmatically with `-o json`

**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

---

### [Tutorial 9: Instance Lifecycle — Instances That Clean Up After Themselves](09-instance-lifecycle.md)
**Duration:** 20 minutes | **Level:** Intermediate

Eliminate forgotten-instance bills with lifecycle management:
- Name instances and connect by name (DNS auto-registration)
- Set TTL, idle-timeout, and on-complete actions
- Send a completion signal from inside your job (`/tmp/SPAWN_COMPLETE`)
- Check status and extend TTL on a live instance

**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

---

### [Tutorial 10: From Capacity to Instance — The truffle → spawn Workflow](10-truffle-to-spawn-workflow.md)
**Duration:** 25 minutes | **Level:** Intermediate

Combine truffle and spawn into a reliable one-liner:
- Scripted cheapest-region selection with `jq`
- Add `--spot` after truffle confirms capacity
- Multi-region fallback loop
- Drop-in GitHub Actions integration

**Prerequisites:** [Tutorials 8](08-finding-ec2-capacity.md) and [9](09-instance-lifecycle.md)

---

### [Tutorial 11: Advanced spawn — Sweeps, Arrays, and Autoscaling](11-advanced-spawn.md)
**Duration:** 30 minutes | **Level:** Advanced

Fleet-scale workflows:
- Mixed-architecture parameter sweeps (Graviton, Intel, AMD)
- Monitor and extend live job arrays
- Autoscale groups that drain SQS queues and scale to zero
- Cost comparison: always-on vs autoscale

**Prerequisites:** [Tutorials 3](03-parameter-sweeps.md), [4](04-job-arrays.md), and [10](10-truffle-to-spawn-workflow.md)

---

## Plugins (Intermediate)

### [Tutorial 12: Private Networking with the Tailscale Plugin](12-tailscale-plugin.md)
**Duration:** 15 minutes | **Level:** Intermediate

Connect instances to your private Tailnet:
- Install the Tailscale plugin with an ephemeral auth key
- SSH and access services via stable 100.x.x.x address
- No security group rules or public IP required
- Remove the plugin and auto-expire the Tailscale node

**Prerequisites:** [Tutorial 2](02-first-instance.md), Tailscale account with auth key

---

### [Tutorial 13: High-Speed Data Transfer with the Globus Plugin](13-globus-plugin.md)
**Duration:** 20 minutes | **Level:** Intermediate

Move large datasets to and from instances using Globus:
- Install the Globus plugin (push flow: local creates endpoint, pushes setup key)
- Transfer datasets from HPC clusters, XSEDE/ACCESS, or another personal endpoint
- Transfer results back with checksum verification
- Remove the plugin and auto-delete the Globus endpoint

**Prerequisites:** [Tutorial 2](02-first-instance.md), globus-cli installed and logged in

---

### [Tutorial 14: Live Directory Sync with the spore-sync Plugin](14-spore-sync-plugin.md)
**Duration:** 20 minutes | **Level:** Intermediate

Keep a local directory in sync with an instance in real time via mutagen:
- Install mutagen locally (the sync engine)
- Start a bi-directional sync session with one command
- Watch edits propagate in both directions within seconds
- Switch sync modes (two-way, one-way-safe, one-way-replica)
- Combine with Tailscale for private-network sync

**Prerequisites:** [Tutorial 2](02-first-instance.md), mutagen installed locally

---

### [Tutorial 15: RStudio Server with Environment Replication](15-rstudio-server-plugin.md)
**Duration:** 25 minutes | **Level:** Intermediate

Launch RStudio Server on a spore instance with your local R environment pre-installed:
- Capture your local renv.lock (or generate one automatically)
- Install RStudio Server and restore packages via renv::restore()
- Log in to RStudio in your browser with the familiar IDE
- Combine with Tailscale to avoid exposing port 8787 publicly

**Prerequisites:** [Tutorial 2](02-first-instance.md), R and renv installed locally

---

## Learning Paths

### Path 1: Quick Start (Get Running Fast)
Perfect for first-time users who want to launch instances quickly.

1. [Tutorial 1: Getting Started](01-getting-started.md) - 15 min
2. [Tutorial 2: Your First Instance](02-first-instance.md) - 20 min

**Total:** 35 minutes

---

### Path 2: ML/Research Workflows
For machine learning and research users running parameter sweeps.

1. [Tutorial 1: Getting Started](01-getting-started.md) - 15 min
2. [Tutorial 2: Your First Instance](02-first-instance.md) - 20 min
3. [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md) - 30 min
4. [Tutorial 6: Cost Management](06-cost-management.md) - 20 min
5. [Tutorial 7: Monitoring & Alerts](07-monitoring-alerts.md) - 30 min

**Total:** 1 hour 55 minutes

---

### Path 3: Batch Processing
For data engineers and batch processing workloads.

1. [Tutorial 1: Getting Started](01-getting-started.md) - 15 min
2. [Tutorial 2: Your First Instance](02-first-instance.md) - 20 min
3. [Tutorial 4: Job Arrays](04-job-arrays.md) - 30 min
4. [Tutorial 5: Batch Queues](05-batch-queues.md) - 45 min
5. [Tutorial 6: Cost Management](06-cost-management.md) - 20 min

**Total:** 2 hours 10 minutes

---

### Path 4: Complete Mastery
Complete all tutorials for comprehensive understanding.

1. [Tutorial 1: Getting Started](01-getting-started.md) - 15 min
2. [Tutorial 2: Your First Instance](02-first-instance.md) - 20 min
3. [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md) - 30 min
4. [Tutorial 4: Job Arrays](04-job-arrays.md) - 30 min
5. [Tutorial 5: Batch Queues](05-batch-queues.md) - 45 min
6. [Tutorial 6: Cost Management](06-cost-management.md) - 20 min
7. [Tutorial 7: Monitoring & Alerts](07-monitoring-alerts.md) - 30 min
8. [Tutorial 8: Finding EC2 Capacity](08-finding-ec2-capacity.md) - 20 min
9. [Tutorial 9: Instance Lifecycle](09-instance-lifecycle.md) - 20 min
10. [Tutorial 10: truffle → spawn Workflow](10-truffle-to-spawn-workflow.md) - 25 min
11. [Tutorial 11: Advanced spawn](11-advanced-spawn.md) - 30 min
12. [Tutorial 12: Tailscale Plugin](12-tailscale-plugin.md) - 15 min
13. [Tutorial 13: Globus Plugin](13-globus-plugin.md) - 20 min
14. [Tutorial 14: spore-sync Plugin](14-spore-sync-plugin.md) - 20 min
15. [Tutorial 15: RStudio Server Plugin](15-rstudio-server-plugin.md) - 25 min

**Total:** 5 hours 45 minutes

---

### Path 5: Cost-Optimized Workflows
For users who want to minimize cost while maximizing reliability.

1. [Tutorial 1: Getting Started](01-getting-started.md) - 15 min
2. [Tutorial 2: Your First Instance](02-first-instance.md) - 20 min
3. [Tutorial 8: Finding EC2 Capacity](08-finding-ec2-capacity.md) - 20 min
4. [Tutorial 9: Instance Lifecycle](09-instance-lifecycle.md) - 20 min
5. [Tutorial 10: truffle → spawn Workflow](10-truffle-to-spawn-workflow.md) - 25 min
6. [Tutorial 11: Advanced spawn](11-advanced-spawn.md) - 30 min

**Total:** 2 hours 10 minutes

---

### Path 6: R Research Environment
For R users who want a cloud RStudio IDE with their local package environment.

1. [Tutorial 1: Getting Started](01-getting-started.md) - 15 min
2. [Tutorial 2: Your First Instance](02-first-instance.md) - 20 min
3. [Tutorial 12: Tailscale Plugin](12-tailscale-plugin.md) - 15 min *(optional, for private access)*
4. [Tutorial 15: RStudio Server Plugin](15-rstudio-server-plugin.md) - 25 min
5. [Tutorial 14: spore-sync Plugin](14-spore-sync-plugin.md) - 20 min *(optional, for live script sync)*

**Total:** 1 hour 35 minutes (core: 1 hour)

---

## What's Next?

After completing tutorials, explore:

🛠️ **[How-To Guides](../how-to/)** - Task-oriented recipes for specific scenarios

📚 **[Command Reference](../reference/)** - Complete command documentation

💡 **[FAQ](../FAQ.md)** - Common questions and troubleshooting

📖 **[Main Documentation](../README.md)** - Full documentation index

---

## Tutorial Format

Each tutorial follows a consistent structure:

**Header:**
- Duration estimate
- Difficulty level
- Prerequisites

**Content:**
- "What You'll Learn" section
- Step-by-step instructions with code examples
- Expected outputs
- Practical exercises
- Real-world examples
- Best practices
- Troubleshooting tips

**Footer:**
- "What You Learned" summary
- Practice exercises
- Next steps and related resources
- Quick reference

---

## Feedback

Found an issue or have suggestions? [Open an issue](https://github.com/spore-host/spore-host/issues/new?labels=type:docs,component:spawn)
