# Frequently Asked Questions (FAQ)

## General

### What is spawn?

spawn is a command-line tool that makes launching AWS EC2 instances effortless. It auto-detects AMIs, creates networking, manages SSH keys, and includes a self-monitoring agent (spored) that auto-terminates instances.

### How much does spawn cost?

spawn itself is free and open-source. You only pay for AWS resources (EC2 instances, EBS volumes). spawn helps reduce costs through:
- Auto-termination (TTL)
- Idle detection
- Spot instance support
- Hibernation

### Do I need to keep my laptop on?

No! The spored agent runs on each instance and handles:
- TTL monitoring
- Idle detection
- Auto-termination
- Spot interruption handling

Your laptop can sleep, disconnect, or shut down.

## Installation & Setup

### How do I install spawn?

**macOS/Linux:**
```bash
curl -LO https://github.com/spore-host/spore-host/releases/latest/download/spawn-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m)
chmod +x spawn-*
sudo mv spawn-* /usr/local/bin/spawn
```

**Build from source:**
```bash
git clone https://github.com/spore-host/spore-host
cd spore-host/spawn
make build && sudo make install
```

### What AWS permissions does spawn need?

Minimal permissions: EC2, IAM, SSM. See [IAM_PERMISSIONS.md](../IAM_PERMISSIONS.md) for complete policy.

**Quick check:**
```bash
./scripts/validate-permissions.sh
```

### How do I configure AWS credentials?

```bash
aws configure
```

Or use environment variables:
```bash
export AWS_PROFILE=myprofile
```

See [Tutorial 1: Getting Started](tutorials/01-getting-started.md#step-2-configure-aws-credentials)

## Launching Instances

### What's the simplest launch command?

```bash
spawn launch --instance-type t3.micro --ttl 1h
```

This launches a t3.micro instance that auto-terminates after 1 hour.

### How do I choose an instance type?

**Development/Testing:** `t3.micro`, `t3.small`
**General Purpose:** `m7i.large`, `m7i.xlarge`
**Compute Intensive:** `c7i.xlarge`, `c7i.2xlarge`
**GPU/ML:** `g5.xlarge`, `g5.2xlarge`

See [Tutorial 2: Instance Types](tutorials/02-first-instance.md#instance-types-choosing-the-right-one)

### Can I use my own AMI?

Yes:
```bash
spawn launch --instance-type t3.micro --ami ami-0abc123def456789
```

Or create custom AMI:
```bash
spawn create-ami <instance-id> --name my-custom-ami
```

### How do I launch in a specific region?

```bash
spawn launch --instance-type t3.micro --region us-west-2
```

Or set default:
```bash
export AWS_REGION=us-west-2
```

### How do I add tags?

```bash
spawn launch --instance-type t3.micro \
  --tags env=dev,project=myapp,owner=alice
```

## Connecting & SSH

### How do I connect to an instance?

```bash
spawn connect <instance-id>
# or
spawn ssh <instance-id>
```

### What if connection fails?

Wait for SSH to be ready:
```bash
spawn connect <instance-id> --wait
```

Or check instance status:
```bash
spawn status <instance-id>
```

### Can I use my own SSH key?

Yes:
```bash
spawn launch --instance-type t3.micro --key-pair my-key
spawn connect <instance-id> --key ~/.ssh/my-key
```

### What if I don't have SSH keys?

spawn creates them automatically on first launch at `~/.ssh/id_rsa`.

Or create manually:
```bash
ssh-keygen -t rsa -f ~/.ssh/id_rsa -N ""
```

## Lifecycle & Costs

### What is TTL?

TTL (Time-To-Live) is the maximum time an instance will run before auto-terminating.

```bash
spawn launch --instance-type t3.micro --ttl 8h
```

### Can I extend TTL?

Yes:
```bash
spawn extend <instance-id> 2h
```

### What is idle timeout?

Idle timeout terminates instances when idle (low CPU, low network):

```bash
spawn launch --instance-type t3.micro --idle-timeout 1h
```

### How much will my instance cost?

Check during launch (estimated):
```bash
spawn launch --instance-type t3.micro
# Shows: $0.0104/hour (~$0.25/day)
```

Or check actual cost:
```bash
spawn cost --instance-id <instance-id>
```

### Does stopping an instance stop charges?

Partially. You stop paying for compute, but still pay for EBS storage (~$0.10/GB/month).

**Better option:** Terminate instance to stop all charges.

### What's the difference between stop and terminate?

- **Stop:** Instance stopped, data preserved, can restart, storage charges continue
- **Terminate:** Instance deleted, all charges stop, cannot restart

### Can I hibernate instances?

Yes, for supported instance types:
```bash
spawn launch --instance-type m7i.large --hibernate --idle-timeout 1h --hibernate-on-idle
```

## Features & Capabilities

### Can I launch multiple instances?

Yes, use job arrays:
```bash
spawn launch --instance-type t3.micro --array 10
```

Or parameter sweeps:
```bash
spawn launch --param-file sweep.yaml
```

### What are parameter sweeps?

Parameter sweeps launch dozens or hundreds of instances with different configurations for hyperparameter tuning or batch processing.

See [Parameter Sweeps Guide](PARAMETER_SWEEPS.md)

### Can I use spot instances?

Yes:
```bash
spawn launch --instance-type t3.micro --spot --spot-max-price 0.05
```

### Does spawn support GPU instances?

Yes, spawn auto-detects GPU instances and uses GPU-enabled AMIs:
```bash
spawn launch --instance-type g5.xlarge
```

### Can I add custom IAM policies?

Yes:
```bash
# Policy templates
spawn launch --instance-type t3.micro --iam-policy s3:ReadOnly,logs:WriteOnly

# Custom policy file
spawn launch --instance-type t3.micro --iam-policy-file policy.json
```

### Can I run scheduled jobs?

Yes:
```bash
spawn schedule create sweep.yaml --cron "0 2 * * *"
```

See [schedule command](reference/commands/schedule.md)

### Can I run sequential batch jobs?

Yes, use batch queues:
```bash
spawn launch --instance-type m7i.large --batch-queue pipeline.json
```

See [queue command](reference/commands/queue.md)

## Troubleshooting

### "Insufficient capacity"

Try:
1. Different availability zone
2. Different region
3. Different instance type
4. Wait and retry

```bash
spawn launch --instance-type t3.micro --region us-west-2
```

### "InvalidKeyPair.NotFound"

Create SSH key:
```bash
ssh-keygen -t rsa -f ~/.ssh/id_rsa -N ""
```

### "UnauthorizedOperation"

Check AWS credentials and permissions:
```bash
aws sts get-caller-identity
```

See [IAM_PERMISSIONS.md](../IAM_PERMISSIONS.md)

### "SSH connection timed out"

Wait for instance to initialize:
```bash
spawn connect <instance-id> --wait
```

Or check security group allows SSH from your IP.

### "Instance not found"

Check instance exists:
```bash
spawn list
```

Or specify region:
```bash
spawn status <instance-id> --region us-west-2
```

### How do I get help?

1. **Documentation:** Read [tutorials](tutorials/) and [how-to guides](how-to/)
2. **Command help:** `spawn launch --help`
3. **GitHub Issues:** [Open an issue](https://github.com/spore-host/spore-host/issues)

## Best Practices

### Should I always set TTL?

Yes! This prevents forgotten instances from accruing charges:
```bash
spawn launch --instance-type t3.micro --ttl 8h
```

### How should I organize instances?

Use consistent tagging:
```bash
--tags env=dev,project=myapp,team=backend,owner=alice
```

### Should I use spot instances?

For non-critical workloads, yes! Save up to 70%:
```bash
spawn launch --instance-type t3.micro --spot
```

### How do I avoid large bills?

1. Always set `--ttl`
2. Use `--idle-timeout` for ML/batch jobs
3. Use spot instances when possible
4. Set budget alerts
5. Monitor costs with `spawn cost`

### Should I use default or custom AMIs?

**Default:** Quick start, always latest
**Custom:** Pre-installed software, faster launch

Create custom AMI:
```bash
spawn create-ami <instance-id> --name my-ami
```

## Advanced Topics

### Can I use spawn with CI/CD?

Yes:
```bash
# GitHub Actions example
- name: Launch instance
  run: |
    INSTANCE_ID=$(spawn launch --instance-type t3.micro --quiet)
    spawn connect "$INSTANCE_ID" -c "run-tests.sh"
```

### Can I integrate with Slurm?

Yes:
```bash
spawn slurm myjob.slurm
```

See [slurm command](reference/commands/slurm.md)

### Can I use spawn across multiple AWS accounts?

Yes, use AWS profiles:
```bash
export AWS_PROFILE=account1
spawn launch --instance-type t3.micro

export AWS_PROFILE=account2
spawn launch --instance-type t3.micro
```

### How does spawn handle spot interruptions?

spored agent monitors spot interruption warnings and:
1. Receives 2-minute warning
2. Saves state/results
3. Gracefully shuts down

Configure behavior:
```bash
spawn launch --spot --spot-interruption-behavior hibernate
```

## Comparison with Other Tools

### spawn vs AWS Console?

**spawn:**
- ✅ Fast (seconds to launch)
- ✅ Repeatable (scripts)
- ✅ Auto-cleanup
- ✅ TTL support

**Console:**
- ✅ Visual interface
- ✅ More options exposed
- ❌ Slower
- ❌ No auto-cleanup

### spawn vs AWS CLI?

spawn wraps AWS CLI with:
- Auto-detection (AMI, networking)
- Simpler commands
- Built-in best practices
- spored agent for self-monitoring

### spawn vs Terraform?

**spawn:** Quick, ephemeral instances
**Terraform:** Infrastructure as code, persistent resources

Use both: Terraform for infrastructure, spawn for temporary compute.

## Getting Help

### Where can I find more documentation?

- **Tutorials:** [docs/tutorials/](tutorials/)
- **How-To Guides:** [docs/how-to/](how-to/)
- **Command Reference:** [docs/reference/](reference/)
- **GitHub:** https://github.com/spore-host/spore-host

### How do I report bugs?

[Open a GitHub issue](https://github.com/spore-host/spore-host/issues/new)

### How do I request features?

[Open a GitHub issue](https://github.com/spore-host/spore-host/issues/new) with the "enhancement" label

### Is there a community?

Check the [GitHub Discussions](https://github.com/spore-host/spore-host/discussions)

---

**Didn't find your question?** [Open an issue](https://github.com/spore-host/spore-host/issues/new?labels=question)
