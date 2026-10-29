# Self-hosted monitoring (Prometheus + Grafana)

`spored` can expose Prometheus metrics, and `deployment/prometheus/` and
`deployment/grafana/` hold working configuration for scraping and displaying
them. This page exists because those twelve files were referenced by no doc, no
Makefile target, no script and no workflow, so the only way to find them was to
list the directory — and because **two defaults mean they do nothing until you
change them.** A reader who copied them and saw empty graphs had no way to tell
which of the two was missing.

## It is opt-in, twice

**1. The metrics server is off.** `pkg/observability.DefaultConfig` sets
`Metrics.Enabled: false`, so `spored` starts no server unless something turns
it on. **2. Discovery needs a tag.** `deployment/prometheus/prometheus.yaml`'s
`spawn-ec2` job uses EC2 service discovery filtered on
`tag:spawn:metrics-enabled = true`, so an instance without that tag is never
scraped: Prometheus reports a healthy config with zero targets.

`spawn launch --metrics` does both. It writes `spawn:metrics-enabled=true`,
which `spored` reads when it starts and which the discovery filter matches
from the first scrape:

```sh
spawn launch my-worker --instance-type c7g.large --metrics --metrics-bind 0.0.0.0
```

`--metrics-bind 0.0.0.0` makes the server listen beyond `localhost` (the
default), so a scraper on another host can reach it. `--metrics-port`
(1024-65535, default 9090) and `--metrics-path` (default `/metrics`) change
the rest; each needs `--metrics`, and a launch with one but not the other is
refused rather than started with a setting that does nothing. These three are
not tags: since v0.150.0 they are in the instance's launch config object, with
the other `spored` settings fixed at launch (#1359), which `spored` reads at
first boot and caches on the instance. `spawn:metrics-enabled` stays a tag
because the discovery filter matches it. `spawn launch --tag` cannot set any
of them, because every `spawn:` key is reserved.

`spored` logs `Starting metrics server on <bind>:<port><path>` when it does start
one, so that line's absence is the check.

### An instance that is already running

The flags apply at launch only. For an instance launched without them, tag it
`spawn:metrics-enabled=true` with the AWS CLI and restart `spored`, which
reads the tag only when it starts; the server then listens on `localhost:9090`.
The bind address, port and path are fixed at launch (a tag with their names is
not read since v0.150.0), so for a scraper on another host use an agent config
file instead (see `example-local-config.yaml`); then the
`spawn:metrics-enabled` tag is needed only for discovery:

```sh
aws ec2 create-tags --region <region> --resources <instance-id> \
  --tags Key=spawn:metrics-enabled,Value=true
spawn connect my-worker -- sudo systemctl restart spored
```


```yaml
observability:
  metrics:
    enabled: true
    bind: 0.0.0.0
    port: 9090
    path: /metrics
```

You also need the scraper to reach the metrics port on the instance, which the
default security group does not allow.

### Tracing and the idle CPU threshold

`--tracing` turns on `spored`'s OpenTelemetry tracing, with spans written to
`spored`'s journal (the `stdout` exporter); `--tracing-sampling` sets the
fraction kept (0 to 1, default 0.1). The `xray` exporter is refused, because
the instance role spawn creates does not grant `xray:PutTraceSegments`, so
every span would be dropped. `--idle-cpu-threshold` sets the CPU percentage
below which `spored` counts the instance as idle (default 5). All three are in
the launch config object, not tags (#1359).

## What is in the tree

| path | what it is |
|---|---|
| `deployment/prometheus/prometheus.yaml` | scrape config: self-monitoring, file-based SD, and EC2 SD with relabelling for region / instance type / spot / job-array tags |
| `deployment/prometheus/alerts/*.yaml` | alert rules — instance, cost, capacity, performance |
| `deployment/prometheus/alertmanager.yaml` | routing template. **The receiver is yours to provide** — see below |
| `deployment/grafana/provisioning/*.yaml` | datasource + dashboard provisioning |
| `deployment/grafana/dashboards/*.json` | fleet monitoring, cost tracking, instance overview, hybrid compute |

## The receiver is BYO

`alertmanager.yaml` used to post to `http://localhost:8080/alertmanager/webhook`,
served by `pkg/observability/alerting` — a webhook bridge with an Alertmanager
payload parser, formatting, and two sender implementations. **Nothing ever
started it**: no command, no entry point, no caller of its `Bridge.Start`. So the
receiver named in the config never existed at runtime and alerts routed to it
went nowhere.

The Go code was removed rather than left to imply a listener (#811). The routing
config and the alert rules are kept, because they are useful against any
receiver, and the placeholder now says so. Point it at your own endpoint, or
replace `webhook_configs` with `slack_configs`, `pagerduty_configs`, or whatever
you run.

## What this is not

Not a deployed part of spore.host. Nothing in the infra account scrapes anything;
`docs/infra-account.md` is the inventory of what actually runs. This is
configuration for an operator who wants to run their own Prometheus against their
own fleet, and it is unsupported in the sense that no CI exercises it.
