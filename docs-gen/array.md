## `spawn array`

Manage job arrays launched with --job-array-name / --count.

Members are grouped by their job-array tags, so these work without any
server-side record:

  spawn array status data-proc
  spawn array logs data-proc --index 3
  spawn array collect data-proc ./results
  spawn array cancel data-proc --pending

```
spawn array
```

### `spawn array cancel`

Terminate a job array's instances

```
spawn array cancel <array-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--pending` |  | bool |  | Only terminate members that are not actively running |
| `--region` |  | string |  | Region to search (default: all regions) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn array collect`

Report where each member's results are (per-index)

```
spawn array collect <array-name> [dest-dir] [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output-dir` |  | string |  | Destination directory hint for results |
| `--region` |  | string |  | Region to search (default: all regions) |

### `spawn array logs`

Fetch the tail of one array member's log.

Selects the member by --index (the sparse job-array index, as shown by
'spawn array status'), then reads /var/log/spawn-command.log (default) or
/var/log/spored.log (--which spored). Uses the instance's SSH key when one is
on disk, else falls back to SSM (keyless/lagotto-launched members).

```
spawn array logs <array-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--index` |  | int |  | Array member index to fetch logs for |
| `--lines` |  | int | `200` | Number of trailing lines to show |
| `--region` |  | string |  | Region to search (default: all regions) |
| `--which` |  | string | `command` | Which log to tail: "command" or "spored" |

### `spawn array status`

Show a job array's members, requested vs launched, and missing indexes

```
spawn array status <array-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | Region to search (default: all regions) |

