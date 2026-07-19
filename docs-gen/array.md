## `spawn array`

Manage job arrays launched with --job-array-name / --count.

Members are grouped by their job-array tags, so these work without any
server-side record:

  spawn array status data-proc
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

### `spawn array status`

Show a job array's members, requested vs launched, and missing indexes

```
spawn array status <array-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--json` |  | bool |  | Output as JSON |
| `--region` |  | string |  | Region to search (default: all regions) |

