# Bring your own app to spawn

`spawn app launch` streams two kinds of app: **DCV** GUI apps and **web** apps
(ones that serve their own HTTP UI — Jupyter, code-server, OpenRefine…). If your
web app has a good public container image, launch it directly — no catalog entry
needed:

```sh
spawn app launch myapp --image someorg/myapp --web-port 8080
```

## When there's no official image

Some apps are actively maintained but ship **no official container image** (and
the community ones are stale). The fix is to build a small, trustworthy image
from the app's official release and publish it publicly. **OpenRefine** is the
worked example — see the full tutorial and a copyable pattern in the
[`spore-host/app-images`](https://github.com/spore-host/app-images) repo.

### The three rules for a web-app image

spawn runs your container on an ephemeral instance, publishes its port on
localhost, and fronts it with **spored's `:443` TLS reverse proxy**, which gates
access with a one-time token (`https://<host>/?spore_token=…` → a
`Secure; HttpOnly` cookie). The token *is* the access control, so:

1. **Bind `0.0.0.0`, not localhost** — the published port must be reachable.
2. **Run auth-less** — the proxy token already gates it (`--web-arg` /the image's
   args turn the app's own login off).
3. **Serve on a known port** — you put it in the catalog entry / `--web-port`.

Access without the token → **403**. Disable the proxy gate only with
`--no-web-auth` (then the app must provide its own auth).

## Launch a built-in web app

The catalog ships a few web apps out of the box — `spawn app list` shows them:

```sh
spawn app launch code-server     # VS Code in the browser
spawn app launch jupyter         # JupyterLab
spawn app launch openrefine      # OpenRefine
```

spawn resolves an instance, pulls the image, waits for the app's port, brings up
the TLS proxy, and opens `https://<host>/?spore_token=…`.

## Add your app to the shipped catalog

To make an app a built-in (visible to everyone in `spawn app list`), add a
`kind: web` entry to the global catalog with a **public** image. See the
[catalog schema reference](catalog-schema.md) for every field. Example
(OpenRefine):

```yaml
- name: openrefine
  kind: web
  port: 3333
  image: ghcr.io/spore-host/openrefine
  tag_default: "3.10.1"
  visibility: public
  gpu: false
  instance_families: [c7i, m7i]
```

No `args:` when the image already binds `0.0.0.0` (as ours does); an app whose
*stock* image binds localhost or requires a token uses `args:` to override — e.g.
code-server's `[--bind-addr, 0.0.0.0:8080, --auth, none]`.

Prefer not to touch the global catalog? Put the same entry in a local overlay
(`~/.spawn/catalog.yaml`) — see
[`catalog-overlay.example.yaml`](catalog-overlay.example.yaml).
