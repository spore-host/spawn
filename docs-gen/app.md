## `spawn app`

Launch streamable research applications in the cloud.

Each application is pre-configured with the right instance type, NICE DCV
for browser-based streaming, and automatic idle termination.

Apps you can launch depend on your account: public images are available to
everyone; private images appear only if your account can pull them. Bring your
own image with --image, or add bindings in ~/.spawn/catalog.yaml (--catalog).

Examples:
  spawn app list                       # show apps launchable from your account
  spawn app launch paraview            # launch ParaView on a GPU instance
  spawn app launch igv --region us-west-2
  spawn app launch paraview --spot --ttl 4h
  spawn app launch paraview --image 123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2

```
spawn app
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--catalog` |  | string |  | Local catalog overlay file (default: $SPAWN_CATALOG or ~/.spawn/catalog.yaml) |

### `spawn app launch`

Launch a catalog application via NICE DCV

```
spawn app launch <app-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--app-version` |  | string |  | App container image tag to launch (default: catalog default; see 'spawn app list') |
| `--idle-timeout` |  | string |  | Stop when DCV has no clients for this duration (default: catalog default) |
| `--image` |  | string |  | Launch a BYO container image for this app (overrides the catalog binding), e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2 |
| `--instance-type` |  | string |  | Override instance type (default: first catalog family + .xlarge) |
| `--name` |  | string |  | Session name (default: <app>-<timestamp>) |
| `--no-open` |  | bool |  | Write session file but do not open browser automatically |
| `--region` |  | string |  | AWS region (default: from AWS config) |
| `--spot` |  | bool |  | Use Spot pricing |
| `--ttl` |  | string |  | Hard termination deadline (e.g. 4h, 8h) |

### `spawn app list`

List all streamable applications in the catalog

```
spawn app list
```

