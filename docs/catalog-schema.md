# Catalog schema reference (`AppEntry`)

The spore.host **application catalog** maps an app name (`paraview`, `igv`,
`jupyter`, …) to the EC2 configuration `spawn app launch` needs: hardware
requirements, the container image or launch command, and how it's streamed to
your browser. The global, public catalog ships embedded in the tools; you can
layer a **local overlay** to add your own apps or rebind an app to an image you
host (the BYO-image model — see
[`catalog-overlay.example.yaml`](catalog-overlay.example.yaml)).

This page documents every field of an `AppEntry` for someone authoring a catalog
entry or a `~/.spawn/catalog.yaml` overlay. The authoritative source is the godoc
in `libs/catalog/catalog.go` (Go module `github.com/spore-host/libs`).

> New to this? Start with [Bring your own app to spawn](bring-your-own-app.md) —
> the how-to (incl. the OpenRefine worked example) — then use this page as the
> field-by-field reference.

## Where entries live

- **Global catalog** — embedded `catalog.yaml` shipped with the tools. Public
  apps only.
- **Local overlay** — `~/.spawn/catalog.yaml`, or a file pointed at by
  `$SPAWN_CATALOG` or `spawn app --catalog <path>`. Merged by name onto the
  global catalog: a matching `name` **rebinds** that app (e.g. supplies an image
  for a recipe-only app); a new `name` **adds** one. Your overlay is private to
  you — entries never appear in anyone else's catalog.

Each YAML document is `apps:` followed by a list of entries:

```yaml
apps:
  - name: myviz
    description: "My lab's visualization tool"
    instance_families: [g6, g5]
    gpu: true
    dcv: true
    image: 123456789012.dkr.ecr.us-east-1.amazonaws.com/myviz
    tag_default: "1.0"
    idle_timeout_default: 20m
```

## Fields

| YAML key | Type | Purpose |
|----------|------|---------|
| `name` | string | Canonical lowercase identifier (e.g. `paraview`, `igv`). Overlay entries merge by this key. **Required.** |
| `description` | string | Short human-readable description, shown in `spawn app list`. |
| `instance_families` | list of string | Recommended EC2 instance families, in preference order (e.g. `[g6, g5]`). The launcher defaults to the first family + `.xlarge`; override with `--instance-type`. |
| `high_vram_families` | list of string | Instance families for large-dataset / high-VRAM workloads. |
| `min_vcpus` | int | Minimum vCPUs required. |
| `min_memory_gib` | int | Minimum memory in GiB required. |
| `gpu` | bool | Whether the app needs a GPU. GPU apps resolve the AWS GPU Deep Learning base AMI (NVIDIA driver preinstalled). |
| `min_vram_gib` | int | Minimum GPU VRAM in GiB (only relevant when `gpu: true`). |
| `dcv` | bool | Back-compat alias: `dcv: true` with no explicit `kind` means `kind: application` (a single GUI app streamed over DCV). |
| `kind` | string | Launch shape: `application`, `desktop`, or `web`. Empty defaults to `application`. See [Launch kinds](#launch-kinds). |
| `port` | int | For a `web` app, the container's HTTP port (e.g. `8888` for Jupyter, `8080` for code-server). **Required when `kind: web`**; ignored otherwise. |
| `health_path` | string | For a `web` app, the HTTP path probed for readiness. Defaults to `/`. |
| `idle_timeout_default` | string | Recommended idle timeout (e.g. `20m`). Applied unless overridden with `--idle-timeout`; the instance stops when idle this long. |
| `launch_command` | string | Full path to the application binary on the AMI, for a **legacy** (non-containerized) app. Prefer `image`. |
| `aliases` | list of string | Alternative names that resolve to this entry (e.g. `pv` → `paraview`). |
| `license` | string | Licensing model: `open-source`, `commercial`, or `needs-conversation`. |
| `image` | string | Container image (without tag) the app runs from, e.g. `public.ecr.aws/spore-host/paraview`. Empty for a not-yet-containerized app. |
| `tag_default` | string | Image tag launched when `--app-version` is not given (e.g. `5.13.2`). |
| `tags_available` | list of string | Image tags a user may select via `--app-version`; used to validate the flag before launch. Always implicitly includes `tag_default`. |
| `visibility` | string | `public` (anonymously pullable by any account) or `private` (needs registry auth + a cross-account grant). When empty it is **inferred** from `image`: `public.ecr.aws/*` → public, `*.dkr.ecr.<region>.amazonaws.com/*` (private ECR) → private. Set it explicitly only to override the guess. |
| `recipe` | string | Path to the **public** build instructions for this app's image (e.g. `infra/amis/containers/paraview` in the spore-host repo). An entry with a `recipe` but no `image` is a buildable *definition*, shown as "recipe available" until someone binds a built image. |
| `base_amis` | map region→AMI ID | **Optional** per-region base-AMI pin. Normally leave unset — see [Base AMIs](#base-amis). |
| `amis` | map region→AMI ID | **Deprecated** (#290): per-app baked AMIs, superseded by container `image` + the SSM-resolved base. Do not set on new entries. |

### Launch kinds

`kind` selects how the app is streamed. Branch behavior:

- **`application`** *(default)* — a single Linux GUI app streamed over an Amazon
  DCV virtual session to a browser tab. This is also what a legacy `dcv: true`
  entry means. Needs `image` (or a legacy `launch_command`).
- **`desktop`** — a bare Linux desktop (GNOME) over DCV, no specific app. Needs
  no `image` or `launch_command`: a desktop environment and DCV are installed at
  boot. Launch with `spawn app launch desktop`.
- **`web`** — an app that serves its **own** web UI on a port (Jupyter,
  code-server, OpenRefine). **No DCV** is installed; `spored` probes `port` and
  fronts the app with a built-in TLS reverse proxy on `:443`, then hands you the
  `https://<host>/` URL. Requires `port`; optionally set `health_path`. You can
  also launch a `web` app ad hoc without a catalog entry:
  `spawn app launch <name> --image <ref> --web-port <n> [--health-path <path>]`.

### Base AMIs

Normally you set **no** AMI fields. The launcher resolves the AWS-maintained base
image via an SSM public parameter at launch — the **GPU Deep Learning base AMI**
(NVIDIA driver preinstalled, present in every region) for a GPU instance type, or
standard Amazon Linux 2023 otherwise — and installs the (free, self-licensing)
Amazon DCV server at boot. There is **no owned or shared "spore-dcv-base" AMI**,
and no per-region AMI table to maintain (spore-host#286/#389).

Set `base_amis` **only** to pin a custom pre-baked image for a region — for
example an AMI that already has your app installed:

```yaml
base_amis:
  us-east-1: ami-0123456789abcdef0   # must be launch-visible from your launch account
```

A pinned AMI wins over SSM resolution for that region. The boot-time DCV install
is idempotent and skips itself when DCV is already present, so pinning an image
that already has DCV is fine.

## See also

- [`docs/catalog-overlay.example.yaml`](catalog-overlay.example.yaml) — a
  copy-paste starting point for a local overlay.
- README **[Launch a GUI or web app](../README.md#launch-a-gui-or-web-app)** —
  the end-user quickstart.
