# DNS updater: who may register a record

`spawn-dns-updater` writes A records into the real `spore.host` zone. This page
records who is allowed to, how that is decided, and the two live steps that are
not part of any deploy.

Measured against the live infra account (966362334030) on 2026-10-11. Re-measure
rather than trusting it.

## Authentication vs authorization

The Function URL is `AuthType: AWS_IAM`, so every request reaching the handler has
passed SigV4 and carries a verified caller account in
`requestContext.authorizer.iam`. The canonical record is namespaced under *that*
account — `{record}.{base36(account)}.spore.host` — so a caller cannot write
outside its own subdomain. That was #173's design and it is sound.

What was missing (#835, GHSA-7pm4-cqjg-vh2q) is that the function's resource
policy is the statement AWS auto-generates for a Function URL:

```json
{
  "Sid": "FunctionURLAllowIAMAccess",
  "Effect": "Allow",
  "Principal": "*",
  "Action": "lambda:InvokeFunctionUrl",
  "Condition": { "StringEquals": { "lambda:FunctionUrlAuthType": "AWS_IAM" } }
}
```

`Principal: "*"` — any principal in any AWS account. SigV4 established *who* the
caller was; nothing established that they were a customer. Since `record_name` is
caller-chosen, a stranger with a free AWS account could publish
`login.<their-base36>.spore.host` pointing anywhere: a phishing primitive hosted
and paid for by spore.host.

## Where the entitled set comes from

An account may register when it is in `DNS_ALLOWED_ACCOUNTS`, or when the portal
account registry **`spore-portal-accounts`** has a row for it that is not
`offboarded` (#1611). That is the same rule dashboard-api applies to every caller
(`lambda/dashboard-api/entitlement.go`). The table is keyed on `accountId`, so the
lookup is a single `GetItem` by the verified account ID that projects `accountId`
and `status` only. The answer is cached per account for 5 minutes (1 minute for a
refusal). A failed read is not cached, and it refuses unless `DNS_ALLOWED_ACCOUNTS`
lists the account.

Until #1611 the lookup was a `Scan` of `spawn-user-accounts`. That table is keyed
on `user_id` with GSIs on `cli_iam_arn` and `email`, so it cannot be read by
account. The `Scan` grant was never on the live role, so from v0.164.0 every
registration logged "account registry unreadable" and fell back to the static
list. A newly onboarded account would have been refused under enforce. There was
also a weaker point: dashboard-api writes `spawn-user-accounts` itself on a
caller's first request, and before #1605 it did so for any caller.

One trap from the old registry still applies to any consumer of these tables:
**an account with live records is in neither registry.** The zone serves four A
records under `c0zxr0ao.spore.host` (`keel-skx`, `keel-zen4`, `keel-zen5`,
`loop-l12-esc`), and `c0zxr0ao` decodes to an account that is not registered.
That account must be in `DNS_ALLOWED_ACCOUNTS`. Ownership questions always derive
base36 from the verified account ID (`encodeAccountID`) and never read it from a
registry.

## Environment variables

| variable | default | effect |
|---|---|---|
| `DNS_AUTHZ_MODE` | *(unset = enforce)* | `audit` logs a would-be refusal (account or record ownership) and continues. Anything else, including unset, makes a refusal a 403. |
| `DNS_ALLOWED_ACCOUNTS` | *(empty)* | Comma-separated account IDs entitled without a registry entry. Malformed entries are reported, not silently dropped. |
| `DNS_ACCOUNTS_TABLE` | `spore-portal-accounts` | The registry to read (keyed on `accountId`). |

Enforce is the default. `update-function-code` does not touch environment
variables and `--environment` *replaces* the whole map, so an unset variable is
the normal outcome of a deploy. That used to argue for audit; since the same
switch now guards record ownership (below), a lost variable must fail closed
rather than reopen every account's records to every principal in it. **Before
deploying a version with this default, entitle every account that must keep
registering without a registry entry in `DNS_ALLOWED_ACCOUNTS`** (the account
behind `c0zxr0ao` above), or set `DNS_AUTHZ_MODE=audit` explicitly.

## Record ownership

The account check says a caller may write under its account's subdomain; it says
nothing about which records there. `lambda/dns-updater/ownership.go` decides that.
The owner of a record is an instance, as IAM identifies it: an instance-profile
session `arn:aws:sts::<account>:assumed-role/<role>/<instance id>` of one of
spawn's instance roles (`spored-instance-role`, `spawn-instance-*`). The owner is
held in the zone as `_spore-instance.<record>` = `"<instance id>"`, written by the
updater in the same batch as the A record (a first claim uses `CREATE`, so two
instances cannot both claim one name), and deleted with it by the updater, the TTL
reaper's DNS teardown and its lease expiry.

- An instance may register a free name, or its own record. It may take over
  another instance's record only once that record's lease is older than
  `pkg/dns.LeaseMaxAge`.
- A record from before ownership (no marker) may be claimed or deleted by an
  instance only when the request comes from the address the record points at.
- Deletes are the owner's only.
- Any other caller (`spawn dns register`/`delete` from a workstation, or an
  instance under a role spawn did not create) may change only records no instance
  owns, and never claims one.

## The IAM grant

`SpawnDNSLambdaExecutionRole` gets two inline policies, written by
`scripts/deploy-custom-dns.sh`. The first is `SpawnDNSAccountRegistryRead`:

```json
{
  "Sid": "ReadAccountRegistryForCallerAuthorization",
  "Effect": "Allow",
  "Action": "dynamodb:GetItem",
  "Resource": "arn:aws:dynamodb:us-east-1:966362334030:table/spore-portal-accounts",
  "Condition": {
    "ForAllValues:StringEquals": { "dynamodb:Attributes": ["accountId", "status"] },
    "Null": { "dynamodb:Attributes": "false" }
  }
}
```

The `Null` check is load-bearing. `ForAllValues:StringEquals` is satisfied by an
**empty** set, so a `GetItem` with no `ProjectionExpression` (which returns the
whole row, including the account's onboarding role and ExternalId) would pass the
attribute condition alone. The Scan this replaced pinned `dynamodb:Select` for the
same reason, but that key exists only on Query and Scan. The request
(`registryGetItemInput`) and the grant are held together by
`lambda/dns-updater/registry_grant_test.go`.

The second is `SpawnDNSAccountRegistryKMS`: `kms:Decrypt` on the table's
customer-managed key, with `kms:ViaService` set to `dynamodb.us-east-1.amazonaws.com`
and `kms:EncryptionContext:aws:dynamodb:tableName` set to `spore-portal-accounts`.
The script reads the key ARN from the table. DynamoDB decrypts an item with the
caller's permissions, so without this grant the `GetItem` fails.

The script's earlier version granted `dynamodb:Scan` on `spawn-user-accounts`
under the same policy name. `put-role-policy` replaces it.

Cases to simulate (`iam simulate-principal-policy` on the role) after applying:

| request | expected |
|---|---|
| `GetItem` on the registry, `dynamodb:Attributes=[accountId,status]` | `allowed` |
| `GetItem` on the registry, `dynamodb:Attributes=[accountId]` | `allowed` |
| `GetItem` also projecting `onboardRoleArn` (or any other attribute) | `implicitDeny` |
| `GetItem` with no `dynamodb:Attributes` context | `implicitDeny` |
| `Scan` / `Query` / `PutItem` on the registry | `implicitDeny` |
| `GetItem` on `spawn-user-accounts` | `implicitDeny` |
| `kms:Decrypt` on the key, via DynamoDB, tableName = the registry | `allowed` |
| `kms:Decrypt` on the key with no `kms:ViaService` | `implicitDeny` |

## The two live steps

Neither is a deploy, and neither is done by any script in this repo.

**1. Narrow the resource policy.** This is the real fix: it refuses at the URL,
before the handler runs, and does not depend on an environment variable.

`add-permission` has no flag for a condition on `aws:PrincipalAccount`, so the
allowlist is expressed as the `--principal` itself: one statement per account.

```sh
# Snapshot first — this is the rollback.
aws lambda get-policy --function-name spawn-dns-updater --region us-east-1 \
  --query Policy --output text > /tmp/dns-updater-policy-before.json

for acct in 435415984226 966362334030 942542972736; do
  aws lambda add-permission --function-name spawn-dns-updater \
    --statement-id "FunctionURLAllow${acct}" \
    --action lambda:InvokeFunctionUrl --principal "$acct" \
    --function-url-auth-type AWS_IAM --region us-east-1
done

# Only after the above succeed for every account:
aws lambda remove-permission --function-name spawn-dns-updater \
  --statement-id FunctionURLAllowIAMAccess --region us-east-1
```

Order matters: add the per-account statements **before** removing the wildcard, or
DNS registration stops for everyone in between.

Note on the third account: it is listed because it owns four live records in the
zone, and listing it **reduces** its access (it can invoke this function today, as
can everyone). It grants spore.host nothing in that account, so it is not a
cross-account trust and not subject to the project's account constraint — but
check that reasoning rather than taking it on faith, and drop the account from the
list if those four hosts are retired.

**2. Turn enforcement on**, once the audit logs confirm which accounts actually
call:

```sh
aws logs filter-log-events --log-group-name /aws/lambda/spawn-dns-updater \
  --filter-pattern "WOULD BE REFUSED" --region us-east-1
```

A `FilterLogEvents` call can return an empty **first page** with a continuation
token, so an empty result is not evidence of no refusals — page it out or widen the
window before concluding anything.

Then set the variable, merging rather than replacing:

```sh
cd lambda/dns-updater && ../../scripts/deploy-custom-dns.sh \
  --hosted-zone-id Z0341053304H0DQXF6U4X --domain spore.host --region us-east-1
# with DNS_AUTHZ_MODE=enforce exported
```

## Deploy path

`scripts/deploy-custom-dns.sh` (the name predates the function). Three things it
got wrong until 2026-10-11, each of which made it unsafe to run:

- it created and granted to `SpawnDNSLambdaRole`, which **does not exist** — the
  function runs under `SpawnDNSLambdaExecutionRole`, so every permission it
  granted went to a role nothing used, and it reported success;
- its update path passed `--environment Variables={HOSTED_ZONE_ID=…,DOMAIN=…}`,
  which would have **deleted `DOMAIN_ZONES`** (the only variable the live function
  has) and written `DOMAIN`, which the handler never reads;
- it parsed, printed and then ignored `--region`, because most of its `aws` calls
  carried no `--region`.
- adding a zone was destructive (fixed in #891). `DEFAULT_DOMAIN`/`HOSTED_ZONE_ID`
  were overwritten with the new zone's, and `put-role-policy` replaced the
  `SpawnDNSPermissions` Route53 grant with one on the new zone alone, revoking
  every earlier zone on a role the script created. The live role also has a
  separate `Route53DNSUpdate` policy for the spore.host zone, which would have
  masked that here. The grant now covers every zone in the merged `DOMAIN_ZONES`, and the
  defaults are only set when absent.
- it never created the Function URL, only read one (fixed in #891). It now
  creates it with `AuthType: AWS_IAM`, refuses any other auth type, and grants
  `lambda:InvokeFunctionUrl` per `--allow-account`. See
  [custom-dns.md](custom-dns.md).

The function was last deployed 2026-06-25 and the code has moved since, so a
`main` that contains a fix is not a production that has one — see the deployed-code
check in `docs/infra-account.md`.
