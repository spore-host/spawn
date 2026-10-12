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

#835 proposed the `spore-portal-accounts` table. It holds **0 items**, so a lookup
against it would authorize nobody.

The populated registry is **`spawn-user-accounts`** (3 items). It is keyed on
`user_id` with GSIs on `cli_iam_arn` and `email`, none of which the updater knows —
it knows an account ID — so the lookup is a `Scan` projecting the single
`aws_account_id` attribute, cached for 5 minutes per container. The table holds one
item per onboarded identity and is small by nature.

Two traps found while implementing this, both worth keeping in mind for any other
consumer of that table:

- **`account_base36` is not reliably the encoding of `aws_account_id`.** The `cli`
  row for 435415984226 stores `c8s8u`, which decodes to 20565246. Derive base36
  from the verified account ID (`encodeAccountID`); never read it from the
  registry.
- **An account with live records is not in the registry.** The zone serves four A
  records under `c0zxr0ao.spore.host` — `keel-skx`, `keel-zen4`, `keel-zen5`,
  `loop-l12-esc` — and `c0zxr0ao` decodes to an account absent from
  `spawn-user-accounts`. Shipping this enforcing would have taken those four hosts
  off the air.

So the entitled set is the union of the registry and `DNS_ALLOWED_ACCOUNTS`, and
enforcement is staged.

## Environment variables

| variable | default | effect |
|---|---|---|
| `DNS_AUTHZ_MODE` | *(unset = audit)* | `enforce` makes a refusal a 403. Anything else logs the would-be refusal and continues. |
| `DNS_ALLOWED_ACCOUNTS` | *(empty)* | Comma-separated account IDs entitled without a registry entry. Malformed entries are reported, not silently dropped. |
| `DNS_ACCOUNTS_TABLE` | `spawn-user-accounts` | The registry to read. |

Audit is the default deliberately. `update-function-code` does not touch
environment variables and `--environment` *replaces* the whole map, so an unset
variable is the normal outcome of a deploy — the unset value therefore has to be
the one that is safe to deploy. It also means **deploying the code does not close
the hole**; the two steps below do.

## The IAM grant

`SpawnDNSLambdaExecutionRole` gets one inline policy, `SpawnDNSAccountRegistryRead`:

```json
{
  "Sid": "ReadAccountRegistryForCallerAuthorization",
  "Effect": "Allow",
  "Action": "dynamodb:Scan",
  "Resource": "arn:aws:dynamodb:us-east-1:966362334030:table/spawn-user-accounts",
  "Condition": {
    "StringEquals": { "dynamodb:Select": "SPECIFIC_ATTRIBUTES" },
    "ForAllValues:StringEquals": { "dynamodb:Attributes": ["aws_account_id"] }
  }
}
```

The `dynamodb:Select` pin is load-bearing, not belt-and-braces:
`ForAllValues:StringEquals` is satisfied by an **empty** set, so a `Scan` naming no
attributes at all passes the `dynamodb:Attributes` condition and returns every
field of every item. Pinning `Select` is what makes a full-table read a denial.
Same family as `ForAllValues:StringNotEquals` on `aws:TagKeys` being defeated by
one innocuous tag key.

It follows that the handler must set `Select` **explicitly** rather than letting
DynamoDB infer it from `ProjectionExpression`: a condition key the request does not
carry evaluates as absent and the condition fails, so inferring it would leave the
grant denying its own only caller — in production and nowhere else.

Simulated with `iam simulate-custom-policy` (2026-10-11):

| request | decision |
|---|---|
| `Scan`, `Select=SPECIFIC_ATTRIBUTES`, `Attributes=[aws_account_id]` | `allowed` |
| `Scan` also projecting `email` | `implicitDeny` |
| `Scan` with `Select=ALL_ATTRIBUTES` | `implicitDeny` |
| `Scan` with no condition context at all | `implicitDeny` |
| `Scan` of a different table | `implicitDeny` |
| `GetItem` / `PutItem` / `DeleteTable` on the registry | `implicitDeny` |

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

The function was last deployed 2026-06-25 and the code has moved since, so a
`main` that contains a fix is not a production that has one — see the deployed-code
check in `docs/infra-account.md`.
