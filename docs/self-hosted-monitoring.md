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
`Metrics.Enabled: false`, so `spored` starts no server unless the agent config
turns it on. The relevant block (see `example-local-config.yaml`):

```yaml
observability:
  metrics:
    enabled: true
    bind: 0.0.0.0
    port: 9090
    path: /metrics
```

`spored` logs `Starting metrics server on <bind>:<port><path>` when it does start
one, so that line's absence is the check.

**2. Nothing sets the discovery tag.** `deployment/prometheus/prometheus.yaml`'s
`spawn-ec2` job uses EC2 service discovery filtered on
`tag:spawn:metrics-enabled = true`. No spawn launch path sets that tag — it is
read in one place (`pkg/provider/ec2.go`) and written nowhere. Tag at launch:

```sh
spawn launch my-worker --instance-type c7g.large \
  --tag spawn:metrics-enabled=true
```

Without it, EC2 service discovery matches nothing and Prometheus reports a
healthy config with zero targets.

You also need the scraper to reach port 9090 on the instance, which the default
security group does not allow.

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
