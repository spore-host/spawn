## `spawn app`

Launch streamable research applications in the cloud.

Each application is pre-configured with the right instance type, NICE DCV
for browser-based streaming, and an automatic stop when idle.

Apps you can launch depend on your account: public images are available to
everyone; private images appear only if your account can pull them. Bring your
own image with --image, or add bindings in ~/.spawn/catalog.yaml (--catalog).

Out of the box, code-server, jupyter, openrefine and desktop launch. The other
apps (paraview, chimerax, qgis, ...) are "recipe available": spore.host ships
the build recipe, and you supply the image you built with --image or a catalog
binding. 'spawn app list' shows which is which for your account.

Examples:
  spawn app list                       # show apps and whether each is launchable
  spawn app launch jupyter             # JupyterLab in the browser
  spawn app launch code-server --region us-west-2
  spawn app launch desktop --spot --ttl 4h
  # A recipe app, with the image you built from its recipe:
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
| `--health-path` |  | string |  | HTTP path probed for web-app readiness (default "/") |
| `--idle-timeout` |  | string |  | Stop when DCV has no clients for this duration (default: catalog default) |
| `--image` |  | string |  | Launch a BYO container image for this app (overrides the catalog binding), e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2 |
| `--instance-type` |  | string |  | Override instance type (default: first catalog family + .xlarge) |
| `--name` |  | string |  | Session name (default: &lt;app&gt;-&lt;timestamp&gt;) |
| `--no-open` |  | bool |  | Write session file but do not open browser automatically |
| `--no-web-auth` |  | bool |  | Disable the spored :443 access-token gate for a web app (the app must provide its own auth) |
| `--region` |  | string |  | AWS region (default: from AWS config) |
| `--spot` |  | bool |  | Use Spot pricing |
| `--ttl` |  | string | `8h` | Hard termination deadline (e.g. 4h, 8h, 1d). Always set: idle only stops the instance, so the TTL is what ends an abandoned session |
| `--web-arg` |  | stringArray |  | Extra argument for a web app's container, repeatable (e.g. --web-arg=--bind-addr=0.0.0.0:8080) |
| `--web-port` |  | int |  | Launch --image as a web-UI app served on this container port (e.g. 8080 for code-server, 8888 for Jupyter); spored fronts it with TLS on :443 |

### `spawn app list`

List all streamable applications in the catalog

```
spawn app list
```

