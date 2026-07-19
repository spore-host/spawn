## `spawn team`

Create and manage teams for sharing spawn instances, sweeps, and autoscale groups

```
spawn team
```

### `spawn team add`

Add a member to a team (owner only)

```
spawn team add <team_id> <iam_arn>
```

### `spawn team create`

Create a new team

```
spawn team create [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--description` |  | string |  | Team description |
| `--name` |  | string |  | Team name (required) |

### `spawn team delete`

Delete a team and all memberships (owner only)

```
spawn team delete <team_id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn team list`

List teams you own or belong to

```
spawn team list
```

### `spawn team remove`

Remove a member from a team (owner only)

```
spawn team remove <team_id> <iam_arn> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn team show`

Show team details and member list

```
spawn team show <team_id>
```

