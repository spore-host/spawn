## `spawn setup-account`

Create (or update) what spawn needs once per AWS account, with
credentials allowed to create IAM policies (an administrator):

  - spore-host/spawn-instance-boundary, the customer-managed permissions
    boundary that caps every IAM role spawn creates in this account. The
    documented spawn user baseline cannot create it on purpose. Until it
    exists, launches that must create or update an instance role are refused,
    naming this command.
  - spore-host/spawn-user-lifecycle, a customer-managed policy granting the
    actions spawn users need on the shared-account lifecycle lease table
    (spore-host-lifecycle), so idle teardown can see the account in use.
    Attach it to the IAM groups or roles your spawn users sign in with, e.g.
    aws iam attach-group-policy --group-name &lt;group&gt; --policy-arn &lt;arn&gt;.

These two are the only things spore.host keeps in an account while it is set
up. Both are permanent account setup, tagged spore-host:managed-by=spawn and
spore-host:permanent=true: no idle teardown or cleanup deletes them, only
--remove. Re-running upgrades either when spawn's document has changed, and is
otherwise a no-op.

--remove takes them away again. It is the only command that deletes either. Without --yes it prints what it would delete and deletes nothing.
It refuses while any role or user still has the boundary as its permissions
boundary (it lists them), and in spore.host's infra account. It detaches the
user lifecycle policy from whatever it is attached to, deletes each policy's
older versions, then the policy, the boundary last.

--offboard (with --remove) also deletes the spore-portal-onboard role that
'spawn onboard' created: the portal's access to this account, and the record
that the account was onboarded, so it is removed only when you ask. A role made
by the portal's CloudFormation quick-create is refused: delete that stack
instead. Offboarding here does not change the portal's own record of the
account (spore-host/spore-host#602).

```
spawn setup-account [flags]
```

**Examples:**

```sh
spawn setup-account                       # provision, or upgrade
  spawn setup-account --remove              # print what removal would delete
  spawn setup-account --remove --yes        # delete the boundary and the user policy
  spawn setup-account --remove --offboard --yes   # ...and the spore-portal-onboard role
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--offboard` |  | bool |  | With --remove, also delete the spore-portal-onboard role (the portal's access to this account) |
| `--region` |  | string |  | AWS region (default: profile/SDK region) |
| `--remove` |  | bool |  | Remove spawn's account setup: the permissions boundary and the user lifecycle policy. Prints the plan; add --yes to delete |
| `--yes` |  | bool |  | With --remove, delete what the plan lists instead of only printing it |

