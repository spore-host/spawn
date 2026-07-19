## `spawn defaults`

Manage default values for spawn launch flags.

Defaults are stored in ~/.spawn/config.yaml and applied whenever the
corresponding flag is not explicitly provided on the command line.

Valid keys:
  slack-workspace    Slack workspace ID for lifecycle notifications (e.g. T03NE3GTY)
  active-processes   Process names to monitor for idle detection (e.g. rsession)
  active-ports       TCP ports to monitor for active connections (e.g. 8787)
  idle-timeout       Default idle timeout duration (e.g. 30m, 1h)
  hibernate-on-idle  Hibernate instead of terminating on idle (true/false)

Examples:
  spawn defaults set slack-workspace T03NE3GTY
  spawn defaults set active-processes rsession
  spawn defaults set idle-timeout 1h
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

