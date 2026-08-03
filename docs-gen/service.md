## `spawn service`

Launch an instance, run a long-lived HTTP service on it, and open a local
tunnel to whatever port the service chose (spawn#409).

The service must print exactly one JSON readiness line to stdout when it starts
serving:

  {"event":"ready","addr":"127.0.0.1:54321","provenance":{"sourceHash":"…"&#125;&#125;

  event   discriminator, so ordinary log lines are ignored
  addr    the address the service ACTUALLY bound, after :0 resolution
  token   optional access credential, carried into the printed URL

The service picks its own port — only it knows what is free — and announces it,
so spawn never polls and hopes. spawn appends "--addr 127.0.0.1:0" to your command
(change it with --addr-args) and forwards a local port to the announced address.

spawn holds the tunnel until you interrupt it or the instance's lifetime ends,
then terminates the instance. The service listens on the instance's loopback and
is reachable only through the tunnel — it is never exposed to the internet.

Examples:
  # Launch an instance, upload a binary, serve it, tunnel to it
  spawn service ./my-server --instance-type m7i.large --upload ./my-server --ttl 2h

  # Run something already baked into the AMI
  spawn service /opt/tools/dashboard --instance-type m7i.large --ttl 30m

  # Use an instance that is already running (it is not terminated afterwards)
  spawn service ./my-server --host my-box --upload ./my-server

  # Preview without launching
  spawn service ./my-server --instance-type m7i.large --ttl 1h --dry-run

Full contract, including how to make a binary spawnable:
https://github.com/spore-host/spawn/blob/main/docs/service-readiness-contract.md

```
spawn service <command> [args...] [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--addr-args` |  | string | `--addr 127.0.0.1:0` | Arguments telling the service where to listen; pass an empty string to append nothing |
| `--ami` |  | string |  | AMI to launch (default: the recommended AMI for the instance type) |
| `--boot-timeout` |  | duration | `0s` | How long to wait for the readiness line (default 2m; raise it for a service that does heavy work before serving) |
| `--dry-run` |  | bool |  | Print the plan without launching anything |
| `--host` |  | string |  | Run on an instance that is already running (name or instance ID); it is not terminated afterwards |
| `--idle-timeout` |  | string |  | Terminate the instance after this much idleness (e.g. 30m) |
| `--instance-type` |  | string |  | EC2 instance type to launch (required unless --host is given) |
| `--key` |  | string |  | SSH private key (default: the instance's launch key) |
| `--local-port` |  | int |  | Local port for the tunnel (default: a free port) |
| `--name` |  | string |  | Name tag for the instance (default: spawn-service) |
| `--open` |  | bool |  | Open the service URL in a browser once it is ready |
| `--remote-path` |  | string |  | Where --upload lands on the instance (default: /tmp/spawn-service-bin) |
| `--security-group-ids` |  | stringSlice |  | Security groups for the instance |
| `--spot` |  | bool |  | Launch as a spot instance (cheaper; an interruption ends the service) |
| `--subnet-id` |  | string |  | Subnet to launch into |
| `--ttl` |  | string |  | Terminate the instance after this long (e.g. 2h). With no --ttl and no --idle-timeout, a 1h idle timeout is applied |
| `--upload` |  | string |  | Local file to copy to the instance before starting (usually the service binary) |
| `--user` |  | string |  | SSH user (default: the instance's spawn:local-username tag, else ec2-user) |

