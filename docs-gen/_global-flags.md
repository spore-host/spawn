### Global flags

These apply to every `spawn` command.

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--accessibility` |  | bool |  | Enable accessibility mode (implies --no-emoji) |
| `--account` |  | string |  | Expected AWS account ID (optional guard) |
| `--lang` |  | string |  | Language for output (en, es, fr, de, ja, pt) |
| `--no-color` |  | bool |  | Disable colorized output |
| `--no-emoji` |  | bool |  | Disable emoji in output |
| `--output` | `-o` | string | `table` | Output format (table, json) |
| `--profile` |  | string |  | AWS named profile (overrides SPORE_PROFILE/AWS_PROFILE and the shared config) |
| `--region` |  | string |  | Default AWS region (overrides SPORE_REGION/AWS_REGION and the shared config) |
| `--verbose` | `-v` | bool |  | Enable verbose output |

