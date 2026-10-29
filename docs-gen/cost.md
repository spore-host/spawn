## `spawn cost`

Display a detailed cost breakdown.

For a parameter sweep or job array, shows the full breakdown:
- Resource costs (compute, storage, network)
- Cloud economics (effective cost/hr, utilization, savings)
- Time breakdown (running vs stopped hours)
- Budget status (if budget was set)
- Cost by region and instance type

For a single instance launched with 'spawn launch' (which has no sweep cost
record), shows an on-the-fly estimate of its cost so far: the on-demand rate
(spawn:price-per-hour tag) × its compute time across every stop and start
(spored's spawn:compute-seconds; compute bills only while running), plus EBS
over the time since first launch when spored has measured it (the
spawn:cost-footprint tag, or spawn:ebs-hourly-cost from an older spored;
EBS bills while stopped too), plus $0.005/hr for each public IPv4 address spored has
recorded (an Elastic IP bills while stopped too). The total is the figure
--cost-limit is enforced against.

Examples:
  spawn cost sweep-20260124-140530
  spawn cost my-devbox

```
spawn cost <sweep-id | instance>
```

