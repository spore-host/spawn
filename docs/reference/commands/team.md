# spawn team

Manage team-based resource sharing for spawn instances, sweeps, and autoscale groups.

## Synopsis

```bash
spawn team <subcommand> [flags]
```

## Description

Teams allow multiple AWS IAM principals to share access to spawn-managed resources. The team owner creates the team and adds members; members can then view and manage shared instances, sweeps, and autoscale groups.

Resources are shared by associating them with a team at launch time using `--team <team-id>`.

## Subcommands

| Subcommand | Description |
|------------|-------------|
| `create` | Create a new team |
| `list` | List teams you own or belong to |
| `show <team-id>` | Show team details and member list |
| `add <team-id> <iam-arn>` | Add a member (owner only) |
| `remove <team-id> <iam-arn>` | Remove a member (owner only) |
| `delete <team-id>` | Delete a team and all memberships (owner only) |

## Usage

### Create a Team

```bash
spawn team create --name ml-team --description "Machine learning research team"
```

### List Teams

```bash
spawn team list
```

### Show Team Details

```bash
spawn team show team-abc123
```

### Add a Member

```bash
spawn team add team-abc123 arn:aws:iam::123456789012:user/alice
```

### Remove a Member

```bash
spawn team remove team-abc123 arn:aws:iam::123456789012:user/alice
```

### Delete a Team

```bash
spawn team delete team-abc123
```

## Using Teams with Launch

Pass `--team <team-id>` to `spawn launch` to associate instances with a team:

```bash
spawn launch \
  --instance-type c5.2xlarge \
  --ami ami-abc123 \
  --team team-abc123 \
  --ttl 4h
```

All team members can then see the instance in `spawn list`.

## Notes

- Teams are stored in DynamoDB (`spawn-teams` and `spawn-team-memberships` tables)
- Only the team owner can add/remove members or delete the team
- Members are identified by IAM ARN (user or role)
- Deleting a team does not terminate associated instances

## See Also

- [spawn launch](launch.md) — Launch instances with `--team` flag
- [spawn list](list.md) — List instances (shows team-shared instances)
