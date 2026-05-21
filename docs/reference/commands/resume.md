# spawn resume

Resume interrupted parameter sweeps.

## Synopsis

```bash
spawn resume --sweep-id <sweep-id> [flags]
```

## Description

Resume a parameter sweep that was interrupted, cancelled, or partially failed. Continues launching instances from the last checkpoint, skipping already-launched parameters.

## Flags

### Required

#### --sweep-id
**Type:** String
**Required:** Yes
**Description:** Sweep ID to resume.

```bash
spawn resume --sweep-id sweep-20260127-abc123
```

### Optional

#### --detach
**Type:** Boolean
**Default:** `false`
**Description:** Resume in detached mode (Lambda orchestration).

```bash
spawn resume --sweep-id sweep-20260127-abc123 --detach
```

#### --max-concurrent
**Type:** Integer
**Default:** Previous value
**Description:** Override maximum concurrent instances.

```bash
spawn resume --sweep-id sweep-20260127-abc123 --max-concurrent 10
```

## Output

```
Resuming sweep: sweep-20260127-abc123

Sweep Status:
  Original Status: interrupted
  Total Parameters: 50
  Already Launched: 28
  Remaining: 22

Resuming from parameter index: 28

Configuration:
  Max Concurrent: 5
  Launch Delay: 5s
  Mode: detached

Resume successful!
Monitor: spawn status --sweep-id sweep-20260127-abc123
```

## Examples

### Resume Basic
```bash
spawn resume --sweep-id sweep-20260127-abc123
```

### Resume in Detached Mode
```bash
spawn resume --sweep-id sweep-20260127-abc123 --detach
```

### Resume with Different Concurrency
```bash
spawn resume --sweep-id sweep-20260127-abc123 --max-concurrent 10
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Resume successful |
| 1 | Resume failed |
| 2 | Invalid sweep ID |
| 3 | Sweep not found |
| 4 | Sweep not resumable (completed or invalid state) |

## See Also

- [spawn launch](launch.md) - Launch sweeps
- [spawn status](status.md) - Check status
- [spawn cancel](cancel.md) - Cancel sweeps
