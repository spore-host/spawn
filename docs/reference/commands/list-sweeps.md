# spawn list-sweeps

List parameter sweeps.

## Synopsis

```bash
spawn list-sweeps [flags]
```

## Description

List all parameter sweeps with filtering by status, date range, and tags.

## Flags

#### --status
**Type:** String
**Allowed Values:** `running`, `completed`, `failed`, `cancelled`, `all`
**Default:** `all`
**Description:** Filter by sweep status.

```bash
spawn list-sweeps --status running
spawn list-sweeps --status completed
```

#### --format
**Type:** String
**Allowed Values:** `table`, `json`
**Default:** `table`
**Description:** Output format.

```bash
spawn list-sweeps --format json
```

#### --since
**Type:** Duration or date
**Default:** Last 30 days
**Description:** Show sweeps since date/duration.

```bash
spawn list-sweeps --since 7d
spawn list-sweeps --since 2026-01-01
```

## Output

### Table Format

```
+---------------------------+-----------+-----------+----------+----------+
| Sweep ID                  | Status    | Progress  | Started  | Cost     |
+---------------------------+-----------+-----------+----------+----------+
| sweep-20260127-abc123     | running   | 28/50     | 2h ago   | $12.45   |
| sweep-20260126-def456     | completed | 50/50     | 1d ago   | $26.70   |
| sweep-20260125-ghi789     | failed    | 25/50     | 2d ago   | $13.20   |
+---------------------------+-----------+-----------+----------+----------+

Total: 3 sweeps
```

### JSON Format

```json
[
  {
    "sweep_id": "sweep-20260127-abc123",
    "status": "running",
    "total_parameters": 50,
    "launched": 28,
    "completed": 23,
    "failed": 0,
    "started_at": "2026-01-27T14:00:00Z",
    "cost": 12.45
  }
]
```

## Examples

### List All Sweeps
```bash
spawn list-sweeps
```

### List Running Sweeps
```bash
spawn list-sweeps --status running
```

### List Recent Sweeps
```bash
spawn list-sweeps --since 7d
```

### JSON Output
```bash
spawn list-sweeps --format json | jq '.[] | select(.status == "running")'
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | List successful |
| 1 | API error |
| 2 | Invalid filter |

## See Also

- [spawn status](status.md) - Check sweep details
- [spawn cancel](cancel.md) - Cancel sweeps
- [spawn resume](resume.md) - Resume sweeps
- [spawn collect-results](collect-results.md) - Collect results
