# spawn dns

Manage DNS records for spawn instances.

## Synopsis

```bash
spawn dns list [flags]
spawn dns delete <dns-name> [flags]
```

## Description

Manage DNS records in the spore.host zone. Instances automatically register on launch if `--dns-name` specified.

## Subcommands

### list
List all DNS records.

```bash
spawn dns list
```

**Output:**
```
+---------------------------+------------------+----------+
| DNS Name                  | Instance ID      | IP       |
+---------------------------+------------------+----------+
| my-instance.c0zxr0ao.s... | i-0123456789abc  | 54.1.2.3 |
| training.c0zxr0ao.spor... | i-0987654321fed  | 52.9.8.7 |
+---------------------------+------------------+----------+
```

### delete
Delete a DNS record.

```bash
spawn dns delete my-instance
```

## Examples

### List All DNS Records
```bash
spawn dns list
```

### Delete DNS Record
```bash
spawn dns delete my-instance
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Operation successful |
| 1 | DNS operation failed |
| 2 | Invalid arguments |

## See Also

- [spawn launch](launch.md) - Set DNS name on launch
- [DNS Setup Guide](../../DNS_SETUP.md) - Configuration
