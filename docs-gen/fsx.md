## `spawn fsx`

Manage FSx Lustre filesystems: list, info, export, delete

```
spawn fsx
```

### `spawn fsx delete`

Delete an FSx Lustre filesystem, optionally exporting to S3 first

```
spawn fsx delete <filesystem-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--export-first` |  | bool |  | Export data to S3 before deleting |
| `--yes` | `-y` | bool |  | Skip confirmation prompt |

### `spawn fsx list`

List all spawn-managed FSx Lustre filesystems across all regions

```
spawn fsx list
```

### `spawn fsx show`

*Aliases: info*

Show detailed information about an FSx Lustre filesystem

```
spawn fsx show <filesystem-id>
```

