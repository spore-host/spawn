## `spawn setup-account`

Create (or update) the customer-managed permissions boundary that caps every
IAM role spawn creates in this account.

Run once per account with credentials allowed to create IAM policies (an
administrator). The documented spawn user baseline cannot create it on purpose.
Until it exists, launches that must create or update an instance role are
refused, naming this command.

Re-running upgrades the boundary when spawn's document has changed, and is
otherwise a no-op.

```
spawn setup-account [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region (default: profile/SDK region) |

