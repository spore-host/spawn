## `spawn onboard`

Create the cross-account role the spore.host portal uses to launch and
manage EC2 in this account, and register it with the portal.

Run with credentials for the account you want to onboard (SSO, profile, or keys).
This is the CLI equivalent of the web CloudFormation quick-create.

Re-running is safe: an existing role keeps the ExternalId it already trusts
(pass --external-id to change it), and its trust and inline policies are
rewritten to the current documents. With -o json, stdout carries only the JSON
result; progress goes to stderr.

```
spawn onboard [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--external-id` |  | string |  | ExternalId the role must trust (default: the existing role's, else a new one) |
| `--phone-home-role` |  | string |  | Portal phone-home Lambda role ARN to trust (or SPORE_PORTAL_PHONE_HOME_ROLE_ARN) |
| `--phone-home-url` |  | string |  | Portal phone-home Function URL (or SPORE_PORTAL_PHONE_HOME_URL) |
| `--region` |  | string |  | AWS region to onboard (default: profile/SDK region) |
| `--skip-phone-home` |  | bool |  | Create the role but don't auto-register with the portal |
| `--spored-role` |  | string |  | Role name to allow iam:PassRole for (default spored-instance-role) |

