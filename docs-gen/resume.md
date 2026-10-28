## `spawn resume`

> **Deprecated:** use 'spawn sweep resume &lt;sweep-id&gt;' instead

Resume an interrupted parameter sweep from checkpoint.

Reads the sweep state from ~/.spawn/sweeps/&lt;sweep-id&gt;.json,
queries EC2 for current instance states, and continues launching
pending parameter sets with rolling queue orchestration.

Examples:
  # Resume sweep with original settings
  spawn resume --sweep-id hyperparam-20260115-abc123

  # Resume with different max-concurrent
  spawn resume --sweep-id &lt;id&gt; --max-concurrent 5

  # Resume in detached mode (Lambda)
  spawn resume --sweep-id &lt;id&gt; --detach

Lifetime bounds: each relaunched member gets the lifetime flags the sweep
was launched with (--ttl, --idle-timeout, --cost-limit, --on-idle,
--no-timeout and the completion settings), and a row's own value still wins.
Passing --ttl, --idle-timeout or --cost-limit here replaces the recorded
value, but may only tighten it. A checkpoint written before spawn recorded
them gets the launch defaults: the row's own bound, else a 1h idle timeout. A
member left with no bound at all is refused.

```
spawn resume [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--cost-limit` |  | float64 |  | Cost limit in USD for each relaunched member, replacing the sweep's --cost-limit; may not be higher than it. A row's own cost_limit: still wins. Not supported with --detach |
| `--detach` |  | bool |  | Run sweep orchestration in Lambda |
| `--idle-timeout` |  | string |  | Idle timeout for each relaunched member, replacing the sweep's --idle-timeout; may not be longer than it. A row's own idle_timeout: still wins. Not supported with --detach |
| `--keep-on-error` |  | bool |  | Keep a relaunched instance whose spored never came up instead of terminating it. It then has no TTL/idle/cost-limit enforcement until spored is confirmed running |
| `--max-concurrent-auto` |  | bool |  | Re-derive max concurrent instances from the account's real AWS quota headroom for the pending parameter sets' instance type(s)/region, instead of the original or a user-supplied number (spawn#494). Not yet supported with --detach. |
| `--max-concurrent` |  | int |  | Override max concurrent instances (0 = use original) |
| `--sweep-id` |  | string |  | Sweep ID to resume (required) |
| `--ttl` |  | string |  | Time to live for each relaunched member (e.g. 4h, 1d), replacing the sweep's --ttl; may not be longer than it. A row's own ttl: still wins. Not supported with --detach |

