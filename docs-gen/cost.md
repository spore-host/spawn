## `spawn cost`

Display a detailed cost breakdown.

For a parameter sweep or job array, shows the full breakdown:
- Resource costs (compute, storage, network)
- Cloud economics (effective cost/hr, utilization, savings)
- Time breakdown (running vs stopped hours)
- Budget status (if budget was set)
- Cost by region and instance type

For a single instance launched with 'spawn launch' (which has no sweep cost
record), shows an on-the-fly compute-cost estimate — the instance's on-demand
rate (spawn:price-per-hour tag) × its runtime — the same figure 'spawn status'
reports (#578).

Examples:
  spawn cost sweep-20260124-140530
  spawn cost my-devbox

```
spawn cost <sweep-id | instance>
```

