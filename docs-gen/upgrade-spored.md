## `spawn upgrade-spored`

Replace the spored lifecycle agent on a running instance with a newer
release WITHOUT terminating/relaunching the instance and without losing spored's
lifecycle state (the TTL deadline, accumulated compute-seconds, and the
completion / pre-stop / idle / FSx config live in EC2 tags that the new spored
re-reads on boot, and the settings fixed at launch in the launch config object
cached on the instance).

An upgrade across v0.150.0, the first spored that reads those settings only from
the launch config object, is refused when the instance keeps them in the other
place: tags on an instance launched by an older spawn, or an object an older
spored would not read.

The default target is this spawn's own version, the spored a launch by this
spawn installs (for a development build of spawn, which has no published spored,
the latest release); pin another with --version. A downgrade is refused unless
--allow-downgrade is given. The swap is driven over SSM
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
| `--version` |  | string |  | Target spored version (e.g. 0.64.0); default: this spawn's version (latest release for a dev build) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

