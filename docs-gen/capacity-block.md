## `spawn capacity-block`

Purchase and manage EC2 Capacity Blocks for ML.

Discover purchasable offerings with 'truffle capacity-blocks', then purchase one
here. After purchase, launch into it with:
  spawn launch <name> --reservation-id <id> --capacity-block --az <reservation-az>

```
spawn capacity-block
```

### `spawn capacity-block purchase`

Purchase a Capacity Block for ML from an offering id (from 'truffle capacity-blocks').

⚠️  A Capacity Block is billed UP FRONT and is NON-REFUNDABLE — the full block
duration is charged at purchase. This is the single most expensive action spawn
can take. The purchase requires you to TYPE three confirmations (the exact price,
'purchase <offering-id>', and an acknowledgement phrase) and refuses to run on a
non-interactive terminal. Use --dry-run first to preview the price and terms
without buying anything.

The offering's instance type, count, and duration must be supplied so the exact
offering can be re-validated (and its current price re-confirmed) immediately
before purchase.

```
spawn capacity-block purchase <offering-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--count` |  | int32 | `1` | Number of instances in the block |
| `--dry-run` |  | bool |  | Preview the price and terms without purchasing (no charge, no write API call) |
| `--duration-hours` |  | int32 |  | Capacity Block duration in hours (required) |
| `--instance-type` |  | string |  | Instance type of the offering, e.g. p5.48xlarge (required) |
| `--platform` |  | string | `Linux/UNIX` | Instance platform (Linux/UNIX, Windows, ...) |
| `--region` |  | string |  | AWS region of the offering (required) |
| `--tag` |  | stringArray |  | Tag to apply to the reservation (key=value; repeatable) |

