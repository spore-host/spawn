# Explanation

Understanding-oriented documentation that clarifies and deepens knowledge about spawn concepts and design decisions.

## What are Explanations?

Explanations answer "why" and "how it works" questions. Unlike tutorials (learning) or how-to guides (tasks), explanations provide background, context, and theoretical understanding.

**When to read explanations:**
- After completing tutorials, want deeper understanding
- Wondering why spawn works the way it does
- Need to make informed architectural decisions
- Want to understand trade-offs

## Available Explanations

### [Architecture Overview](architecture.md)
**Understanding spawn's system design**

Topics covered:
- Component architecture (CLI, spored agent, AWS resources)
- Data flow (launch, TTL enforcement, spot interruption)
- Security model (authentication, authorization, network)
- Scalability characteristics
- Failure modes and resilience
- Design decisions and rationale

**Read this if:**
- You want to understand how spawn works under the hood
- You're architecting a system using spawn
- You're contributing to spawn development
- You need to debug complex issues

### [Core Concepts](core-concepts.md)
**Deep dive into TTL, idle detection, and spot interruption handling**

Topics covered:
- Time-To-Live (TTL): countdown, warnings, extensions
- Idle detection: algorithm, thresholds, pitfalls
- Spot interruption: monitoring, cleanup actions, user patterns
- Interaction between concepts
- Common scenarios and edge cases

**Read this if:**
- You want to master TTL and idle detection
- You're using spot instances
- You need to optimize instance lifecycle
- You're hitting TTL/idle edge cases

### [Security Model](security-model.md)
**Understanding spawn's security architecture**

Topics covered:
- Threat model (assets, threat actors)
- Authentication & authorization (user, instance)
- Network security (default vs secure configuration)
- Secrets management (anti-patterns, secure patterns)
- Data protection (at rest, in transit)
- IAM policy hardening (least privilege)
- Audit logging (what, how, querying)
- Compliance (HIPAA, PCI DSS, SOC 2)
- Incident response procedures

**Read this if:**
- You're handling sensitive data
- You need compliance certification
- You're securing production workloads
- You need to respond to security incidents

### [Cost Optimization Theory](cost-optimization.md)
**Understanding the economics of ephemeral compute**

Topics covered:
- Cost model (compute, storage, network)
- Cost drivers (runtime, instance type, on-demand vs spot, regions)
- Optimization strategies (TTL, idle detection, right-sizing, spot, scheduling, batching, regional arbitrage)
- Cost anti-patterns (forgotten instances, overprovisioning, avoiding spot)
- Cost monitoring (metrics, alerting)
- Total cost of ownership (cloud vs on-premises)

**Read this if:**
- You want to minimize AWS bills
- You're optimizing cloud spending
- You need to justify spawn vs alternatives
- You're setting budget policies

## How Explanations Relate to Other Docs

```
Learning Path:
1. Tutorials (learning-oriented)
   └─> Learn by doing, step-by-step
2. How-To Guides (task-oriented)
   └─> Solve specific problems
3. Explanations (understanding-oriented)  ← You are here
   └─> Understand why and how
4. Reference (information-oriented)
   └─> Look up details
```

**Example learning journey:**
```
1. Tutorial: "Your First Instance"
   └─> Learn to launch instance with TTL

2. How-To: "Cost Optimization"
   └─> Apply TTL strategies to reduce costs

3. Explanation: "Core Concepts - TTL"
   └─> Understand how TTL countdown works,
       warning system, extension mechanics

4. Reference: "spawn extend command"
   └─> Look up exact syntax for extending TTL
```

## Topics Not Yet Covered

The following explanation documents are planned but not yet written:

- **Multi-Account Architecture** - Cross-account patterns, Organizations setup
- **spored Agent Design** - Agent internals, systemd integration, state machine
- **DNS System** - Route53 integration, subdomain scheme, Lambda functions
- **Parameter Sweep Theory** - Cartesian products, random search, optimization
- **Job Queue Theory** - DAG execution, dependency resolution, retry logic

## Contributing

Found something confusing? Think an explanation could be clearer? Please [open an issue](https://github.com/spore-host/spore-host/issues/new?labels=type:docs,component:spawn).

## Related Documentation

- **[Tutorials](../tutorials/)** - Learn spawn step-by-step
- **[How-To Guides](../how-to/)** - Solve specific tasks
- **[Reference](../reference/)** - Look up command syntax
- **[Architecture Overview](architecture.md)** - Start with system design
