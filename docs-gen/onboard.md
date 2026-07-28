## `spawn onboard`

Create the cross-account role the spore.host portal uses to launch and
manage EC2 in this account, and register it with the portal.

Run with credentials for the account you want to onboard (SSO, profile, or keys).
This is the CLI equivalent of the web CloudFormation quick-create.

```
spawn onboard [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--external-id` |  | string |  | Reuse a specific ExternalId (default: generate a new one) |
| `--phone-home-role` |  | string |  | Portal phone-home Lambda role ARN to trust (or SPORE_PORTAL_PHONE_HOME_ROLE_ARN) |
| `--phone-home-url` |  | string |  | Portal phone-home Function URL (or SPORE_PORTAL_PHONE_HOME_URL) |
| `--region` |  | string |  | AWS region to onboard (default: profile/SDK region) |
| `--skip-phone-home` |  | bool |  | Create the role but don't auto-register with the portal |
| `--spored-profile` |  | string |  | Instance profile name to allow PassRole for (default spored-instance-profile) |

