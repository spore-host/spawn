## `spawn logs`

Fetch a spore's log.

While the instance is alive this tails the log directly, over SSH when a local key
resolves and over SSM otherwise — the same path 'spawn array logs' uses.

Once the instance is GONE, that is impossible: the log died with it. spored
writes the last 50 lines of a FAILED job's command log to the serial console
before terminating (#736), and this reads it back out, so "why did my job fail?"
is answerable after the fact without knowing that get-console-output exists.

The serial console capture is not immediate — allow about five minutes after
termination. Fetched dumps are cached under ~/.spawn/cache/console for 7 days and
pruned on each run.

```
spawn logs <name-or-instance-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--console` |  | bool |  | Print the whole serial console dump, not just the command-log block |
| `--lines` |  | int | `100` | Lines to tail from a LIVE instance (the console block is fixed at what spored wrote) |
| `--no-cache` |  | bool |  | Ignore the local cache and re-fetch from AWS |
| `--region` |  | string |  | AWS region (default: resolved as for other commands) |
| `--which` |  | string | `command` | Which log: command or spored |

