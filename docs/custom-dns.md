# Custom DNS

By default every spore registers `<name>.<base36-account>.spore.host` through
spore.host's DNS updater. A custom deployment registers under your own zone
instead, such as `<name>.<base36-account>.spore.example.edu`, through an updater
you run.

spored registers the record itself, at boot, and renews a lease beside it about
hourly (`_spore-lease.<record>`, defined in `pkg/dns/lease.go`). Who may call an
updater is covered in [dns-updater-authorization.md](dns-updater-authorization.md). The launch tells spored where to
register by writing two tags beside `spawn:dns-name`:

| tag | meaning |
|---|---|
| `spawn:dns-domain` | the zone to register under |
| `spawn:dns-endpoint` | the updater's Lambda Function URL |

Neither tag is written for spore.host's defaults.

## 1. Deploy an updater

```bash
./scripts/deploy-custom-dns.sh \
  --hosted-zone-id Z0123456789ABCDEFGHIJ \
  --domain spore.example.edu \
  --allow-account 111122223333 --allow-account 444455556666
```

The script builds `lambda/dns-updater` and grants it Route53 on the zone. It
creates a Function URL with `AuthType: AWS_IAM`, and gives each
`--allow-account` (default: the deploying account) `lambda:InvokeFunctionUrl`.
It also adds those accounts to the updater's `DNS_ALLOWED_ACCOUNTS`.

It deploys into account 966362334030 unless `SPAWN_LAMBDA_ACCOUNT` names
another. That's deliberate: run against spore.host's own updater, it **adds** your
zone to it. To run your own updater, set
`SPAWN_LAMBDA_ACCOUNT=<your account>` (and `SPAWN_DNS_ROLE_NAME` if you want a
different role name).

Run against an updater that already serves other zones, the script is additive.
`DOMAIN_ZONES` gains the zone, the Route53 grant covers every zone served, the
default zone and `DNS_AUTHZ_MODE` are left alone, and the account allowlist only
grows.

The updater is reached only through its Function URL. The script grants
`lambda:InvokeFunctionUrl` and never `lambda:InvokeFunction`. A direct invocation
lets the caller write the whole event, including the caller-account field the
updater trusts, so it would let one allowed account register records as another.

## 2. Point launches at it

Each value resolves independently, first match wins:

| | domain | endpoint |
|---|---|---|
| flag | `--dns-domain` | `--dns-api-endpoint` |
| environment | `SPAWN_DNS_DOMAIN` | `SPAWN_DNS_API_ENDPOINT` |
| `~/.spawn/config.yaml` | `dns.domain` | `dns.api_endpoint` |
| SSM, in the launch's account and region | `/spawn/dns/domain` | `/spawn/dns/api_endpoint` |
| default | `spore.host` | spore.host's updater |

SSM is the way to set it once for everyone launching in an account:

```bash
aws ssm put-parameter --region us-east-1 --type String \
  --name /spawn/dns/domain --value spore.example.edu
aws ssm put-parameter --region us-east-1 --type String \
  --name /spawn/dns/api_endpoint --value https://<id>.lambda-url.us-east-1.on.aws/
```

Both are validated before anything is launched. The domain must be a lowercase
domain name. The endpoint must be an `https://<id>.lambda-url.<region>.on.aws/`
Function URL: requests are SigV4-signed for Lambda in the region the hostname
names, so no other kind of endpoint can authenticate. spored validates the tags
again before using them. On a bad value it registers nothing and sets
`spawn:dns-status=failed` with the reason. It never falls back to spore.host.

A domain alone is fine when the updater you already use serves that zone. An
endpoint alone sends requests for spore.host to that updater.

`spawn launch --dry-run` shows the target (`DNS: web under spore.example.edu`,
`DNS updater: …`), and `-o json` carries `dns_domain` / `dns_endpoint`.

## 3. Check it

```bash
spawn launch dns-test --ttl 1h --dns-domain spore.example.edu \
  --dns-api-endpoint https://<id>.lambda-url.us-east-1.on.aws/
spawn status dns-test        # spawn:dns-status: registered
spawn dns list               # FQDNs in each instance's own zone
```

`spawn dns register` and `spawn dns delete` act where the instance's tags say
spored registered it, unless `--domain` / `--endpoint` say otherwise.

## Limits

- **The TTL reaper must be told about the zone.** A spore that terminates
  cleanly deletes its own record. One that dies without doing so (a crash, an
  out-of-band terminate) leaves it, and only the TTL reaper's DNS work —
  teardown, the reconciliation sweep, lease expiry — removes it. That covers
  the zone in `DnsZoneId` plus every zone in `DnsExtraZoneIds` (#1065), so add
  your zone's ID there, or run a reaper of your own with your zone as
  `DnsZoneId`:

  ```bash
  cd lambda/ttl-reaper && make deploy DNS_EXTRA_ZONE_IDS=Z0123456789ABCDEFGHIJ
  ```

  `make deploy` keeps every other live parameter, so that is the only change;
  comma-separate several IDs. The reaper reads each zone's domain from Route53 at startup and logs
  `DNS zones covered: …`. A zone it cannot read is logged as `NOT covered`, and
  the others still apply. A spore registered in a zone the reaper doesn't
  cover is logged at teardown and left alone. Until then, clean up with
  `spawn dns delete`.
- **Paths that bypass the launch preflight.** `spawn launch`, its `--count`
  arrays and `--mpi` cohorts resolve the target. Launches built elsewhere
  (batch queue, parameter sweeps, `spawn burst`) register under spore.host as
  before.
