# Plugin Authoring Guide

A spore-host plugin is a `plugin.yaml` file that describes how to install, configure, start,
and health-check a service on a running EC2 instance. Plugins are fetched from GitHub and
executed by the `spored` daemon on the instance, with the controller (`spawn`) orchestrating
local provisioning steps.

---

## Quick start

```yaml
name: my-service
version: "1.0.0"
description: "Installs and runs my-service on EC2"

remote:
  install:
    - type: run
      run: curl -fsSL https://example.com/install.sh | sh

  start:
    - type: run
      run: systemctl start my-service

  stop:
    - type: run
      run: systemctl stop my-service

  health:
    interval: 5m
    steps:
      - type: run
        run: systemctl is-active --quiet my-service
```

---

## Plugin spec reference

### Top-level fields

| Field | Required | Description |
|---|---|---|
| `name` | ✓ | Plugin identifier. Pattern: `[a-zA-Z0-9_-]{1,64}` |
| `version` | ✓ | Semantic version string, e.g. `"1.0.0"` (no `v` prefix) |
| `description` | ✓ | One-line human description |
| `config` | | User-configurable parameters (see below) |
| `conditions` | | Pre-flight checks before install begins |
| `local` | | Steps that run on the controller machine |
| `remote` | | Steps that run on the EC2 instance via spored |
| `outputs` | | Declare values captured and exposed by this plugin |

---

### `config` block

Declares parameters the user supplies at install time via `--config key=value`.

```yaml
config:
  auth_key:
    required: true
    type: string        # string | int | bool
  collection_path:
    required: false
    type: string
    default: /home      # must be set if required: false
```

- Optional parameters **must** have a `default`.
- Values are available as `{{ config.<key> }}` in all step templates.

---

### `conditions` block

Pre-flight checks that must pass before installation begins.

```yaml
conditions:
  local:                # run on the controller (spawn)
    - type: command
      run: which globus
      message: "globus CLI required: pip install globus-cli"
    - type: command
      run: globus whoami
      message: "not logged in: run 'globus login'"
  remote:               # run on the instance via spored
    - type: platform
      os: linux
      message: "this plugin requires Linux"
```

**Condition types:**

| Type | Fields | Description |
|---|---|---|
| `command` | `run`, `message` | Command must exit 0 |
| `platform` | `os` (`linux`, `darwin`, `windows`), `message` | OS must match |

---

### `local` block

Steps that run on the controller machine (where `spawn` is invoked).

```yaml
local:
  provision:    # runs before remote install
    - type: run
      run: globus endpoint create "{{ config.endpoint_name }}" --format json
      capture:
        setup_key: ".setup_key"
        endpoint_id: ".id"
    - type: push
      key: setup_key
      value: "{{ outputs.setup_key }}"
  deprovision:  # runs after remote uninstall
    - type: run
      run: globus endpoint delete "{{ outputs.endpoint_id }}"
```

---

### `remote` block

Steps executed on the instance by the `spored` agent.

```yaml
remote:
  install:    # install software; runs once
  configure:  # configure after install; runs after push API delivers keys
  start:      # start the service
  stop:       # stop the service (called on plugin remove or instance shutdown)
  health:
    interval: 5m   # Go duration: 30s, 5m, 1h
    steps:         # run periodically; non-zero exit → degraded after 3 failures
      - type: run
        run: systemctl is-active --quiet my-service
```

**Lifecycle order:** `install` → *(push API delivers keys)* → `configure` → `start` → running

---

### Step types

#### `run` — Execute a shell command

```yaml
- type: run
  run: |
    curl -fsSL https://example.com/install.sh | sh
  env:
    MY_VAR: "{{ config.param }}"
  background: false    # set true to run detached (fire and forget)
  capture:
    token: ".access_token"   # JMESPath into stdout JSON → outputs.token
```

- The command runs in `bash -c`.
- `capture` extracts values from **JSON** stdout. Each entry maps an output name to a JMESPath expression.
- Captured values are available as `{{ outputs.<name> }}` in subsequent steps.

#### `fetch` — Download a file

```yaml
- type: fetch
  url: https://downloads.example.com/app-latest.tgz
  dest: /tmp/app.tgz
```

#### `extract` — Extract an archive

```yaml
- type: extract
  src: /tmp/app.tgz
  dest: /opt/app
```

Supports `.tar.gz`, `.tgz`, `.tar.bz2`, `.zip`.

#### `push` — Push a value from controller to instance  *(local block only)*

```yaml
- type: push
  key: setup_key
  value: "{{ outputs.setup_key }}"
```

Sends the value through the SSH-tunneled push API on the instance. The instance stores it in
`pushed.<key>` and transitions from `waiting_for_push` to `configuring` once the push is
received (or the configure step explicitly references `{{ pushed.<key> }}`).

---

## Template variables

All `run`, `fetch`, `extract`, and `push` fields support Go template syntax.

| Variable | Description |
|---|---|
| `{{ instance.id }}` | EC2 instance ID, e.g. `i-0abc1234` |
| `{{ instance.name }}` | Instance `Name` tag value |
| `{{ instance.ip }}` | Public IP address |
| `{{ config.<key> }}` | User-supplied config parameter |
| `{{ outputs.<key> }}` | Value captured by a prior `run` step with `capture` |
| `{{ pushed.<key> }}` | Value pushed via the push API from the controller |

> **Shell quoting:** config, outputs, and pushed values are single-quoted in `run` steps to
> prevent injection. Instance values are trusted and left unquoted.

---

## The push flow

Use the push flow when the service requires a key or token that the **controller generates
after** remote installation completes — for example, an endpoint setup key that is created
by calling a cloud API on the user's behalf.

```
controller                          instance
──────────                          ────────
spawn plugin install ...
  └─ remote.install runs                ✓ software installed
  └─ local.provision runs
       ├─ generate/fetch key
       └─ push key to instance          ✓ key received (waiting_for_push → configuring)
  └─ remote.configure runs              ✓ {{ pushed.key }} available
  └─ remote.start runs                  ✓ service running
```

Example (Globus):

```yaml
local:
  provision:
    - type: run
      run: globus endpoint create "{{ config.endpoint_name }}" --format json
      capture:
        setup_key: ".setup_key"
    - type: push
      key: setup_key
      value: "{{ outputs.setup_key }}"

remote:
  configure:
    - type: run
      run: globusconnectpersonal -setup "{{ pushed.setup_key }}"
```

---

## Plugin reference formats

```bash
# Official registry (scttfrdmn/spore-plugins, main branch)
spawn plugin install tailscale

# Official registry, pinned to a tag
spawn plugin install tailscale@v1.2.0

# Custom GitHub repository
spawn plugin install github:myorg/my-plugins/myplugin

# Custom GitHub, pinned to tag
spawn plugin install github:myorg/my-plugins/myplugin@v2.0.0

# Local file (development)
spawn plugin install ./path/to/plugin.yaml
```

For a GitHub ref `github:owner/repo/name[@version]`, the plugin is fetched from:
```
https://raw.githubusercontent.com/<owner>/<repo>/<version-or-main>/<name>/plugin.yaml
```

---

## Authoring checklist

Before publishing a plugin:

- [ ] `name` matches the directory name in the repository
- [ ] `version` is a semver string (no `v` prefix), e.g. `"1.0.0"`
- [ ] All optional `config` params have a `default`
- [ ] All `conditions` have a `message` explaining what to do on failure
- [ ] `remote.install` and `remote.start` are non-empty
- [ ] `remote.stop` gracefully shuts down the service
- [ ] `health.steps` exit 0 when healthy, non-zero when not
- [ ] `capture` steps output JSON; tested with `echo '...' | jq .`
- [ ] `validate_test.go` passes: `go test ./...`
- [ ] Tested end-to-end on AL2023 (`amazon/al2023-ami-*`)

---

## Reference plugins

| Plugin | Repo | Install command |
|---|---|---|
| Tailscale ephemeral node | [spore-host-plugin-tailscale](https://github.com/spore-host/spore-host-plugin-tailscale) | `spawn plugin install github:spore-host/spore-host-plugin-tailscale/tailscale` |
| Globus Connect Personal | [spore-host-plugin-globus](https://github.com/spore-host/spore-host-plugin-globus) | `spawn plugin install github:spore-host/spore-host-plugin-globus/globus-connect-personal` |
