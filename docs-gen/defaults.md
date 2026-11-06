## `spawn defaults`

Manage default values for spawn launch flags.

Defaults are stored in ~/.spawn/config.yaml and applied whenever the
corresponding flag is not explicitly provided on the command line.

Valid keys:
  slack-workspace    Slack workspace ID for lifecycle notifications (e.g. T03NE3GTY)
  active-processes   Process names to monitor for idle detection (e.g. rsession)
  active-ports       TCP ports to monitor for active connections (e.g. 8787)
  idle-timeout       Default idle timeout duration (e.g. 30m, 1h)
  hibernate-on-idle  Hibernate instead of stopping on idle (true/false)
  bootstrap-egress   --bootstrap-egress for --no-public-ip launches (none/proxy)
  compute-profile    AWS profile for the compute account, where instances and
                     detached sweep members launch, when SPAWN_COMPUTE_PROFILE
                     is unset. Unset, spawn uses your own profile (--profile /
                     AWS_PROFILE) or the ambient credentials. For split setups
                     that launch in a dedicated account (e.g. spore-host-dev).

Examples:
  spawn defaults set slack-workspace T03NE3GTY
  spawn defaults set active-processes rsession
  spawn defaults set idle-timeout 1h
  spawn defaults set compute-profile spore-host-dev
  spawn defaults list
  spawn defaults unset active-processes

```
spawn defaults
```

### `spawn defaults list`

List all default launch values

```
spawn defaults list
```

### `spawn defaults set`

Set a default launch value

```
spawn defaults set <key> <value>
```

### `spawn defaults unset`

Remove a default launch value

```
spawn defaults unset <key>
```

