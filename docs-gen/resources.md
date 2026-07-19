## `spawn resources`

List every AWS resource spore.host created in an account/region, found by
the spawn:managed=true tag via the Resource Groups Tagging API.

By default it lists resources created by you (your IAM principal). Use --all to
include resources created by other principals in the account.

```
spawn resources [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-regions` |  | bool |  | Search every enabled region |
| `--all` |  | bool |  | Include resources created by other principals (default: only yours) |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |

