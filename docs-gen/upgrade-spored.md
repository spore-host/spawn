## `spawn upgrade-spored`

Replace the spored lifecycle agent on a running instance with a newer
release WITHOUT terminating/relaunching the instance and without losing spored's
lifecycle state (the TTL deadline, accumulated compute-seconds, and the
completion / pre-stop / idle / FSx config all live in EC2 tags that the new
spored re-reads on boot).

The default target is the latest released version; pin one with --version. A
downgrade is refused unless --force is given. The swap is driven over SSM
(keyless — works on private-subnet / no-public-IP instances), so the instance
must have the SSM agent online and an instance profile (the spored role attaches
AmazonSSMManagedInstanceCore, so spawn-launched instances already qualify).

The TTL deadline is absolute and tag-stored, so it is NOT reset by the restart —
an instance mid-life keeps its original termination time. Linux only for now
(Windows spored upgrade is a follow-up, #234).

```
spawn upgrade-spored <instance-id|name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--allow-downgrade` |  | bool |  | Allow a downgrade (target older than the running version) |
| `--timeout` |  | duration | `5m0s` | How long to wait for the on-instance upgrade to complete |
| `--version` |  | string |  | Target spored version (e.g. 0.64.0); default: latest release |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

