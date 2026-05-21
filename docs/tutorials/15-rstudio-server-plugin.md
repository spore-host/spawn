# Tutorial 15: RStudio Server with Environment Replication

**Duration:** 25 minutes | **Level:** Intermediate | **Issue:** [#213](https://github.com/spore-host/spore-host/issues/213)

---

## What You'll Learn

- Install RStudio Server on a spore instance using the rstudio-server plugin
- Automatically replicate your local R/renv environment to the instance
- Access RStudio Server in your browser
- Verify that your R packages are available on the remote instance
- Stop and restart the RStudio Server service
- Security considerations for production use

**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md), R and renv installed locally

---

## Background

The rstudio-server plugin installs [RStudio Server](https://posit.co/products/open-source/rstudio-server/) on a spore instance and replicates your local R environment using [renv](https://rstudio.github.io/renv/).

**How it works:**

1. **Local provision**: Captures your `renv.lock` (or generates one), base64-encodes it, and pushes it to the instance via the plugin push API
2. **Remote install**: Installs R, RStudio Server, and the `renv` package on the instance
3. **Remote configure**: Decodes the lockfile, runs `renv::restore()` to install all packages, sets the RStudio login password
4. **Remote start**: Starts the `rstudio-server` systemd service

The push flow means the instance waits after installation until the lockfile arrives — ensuring package restore happens before you log in.

---

## Step 1: Ensure R and renv are Installed Locally

```bash
# Check R
Rscript --version
# R scripting front-end version 4.3.x

# Check/install renv
Rscript -e 'if (!requireNamespace("renv")) install.packages("renv")'
```

---

## Step 2: Create a Project with renv

If you have an existing project with `renv.lock`, skip to Step 3.

```bash
mkdir -p ~/r-project && cd ~/r-project

# Initialize renv
Rscript -e 'renv::init()'

# Install some packages
Rscript -e 'install.packages(c("ggplot2", "dplyr", "tidyr"))'

# Snapshot the environment
Rscript -e 'renv::snapshot()'
```

You should now have `renv.lock` in `~/r-project/`.

---

## Step 3: Launch a Spore Instance

RStudio Server requires a reasonably sized instance. At minimum, `t3.medium` (4 GB RAM) is recommended; for real workloads use `r6i.large` or larger.

```bash
spawn launch \
  --name rstudio-demo \
  --instance-type r6i.large \
  --ttl 4h
```

Note the instance ID and IP:

```
Launched: i-0abc123def456789
IP:       54.12.34.56
DNS:      rstudio-demo.spore.host
```

---

## Step 4: Open Port 8787

RStudio Server listens on port 8787. Make sure your security group allows inbound TCP on that port from your IP (or use Tailscale — see [Tip: Private Access via Tailscale](#tip-private-access-via-tailscale) below).

```bash
# If using a named security group created by spawn:
aws ec2 authorize-security-group-ingress \
  --group-name spawn-default \
  --protocol tcp \
  --port 8787 \
  --cidr $(curl -s https://checkip.amazonaws.com)/32
```

---

## Step 5: Install the rstudio-server Plugin

Run from inside your project directory (so the plugin finds `renv.lock`):

```bash
cd ~/r-project

spawn plugin install github:spore-host/spore-host-plugin-rstudio/rstudio-server \
  --instance i-0abc123def456789 \
  --config password=my-secure-password
```

Expected output:

```
[rstudio-server] Checking conditions...
[rstudio-server] ✓ Rscript found at /usr/local/bin/Rscript
[rstudio-server] Running local provision...
[rstudio-server] ✓ renv.lock found (47 packages)
[rstudio-server] ✓ R version detected: 4.3.2
[rstudio-server] ✓ Lockfile encoded and queued for push
[rstudio-server] Installing on instance (this may take 5–10 minutes)...
[rstudio-server] ✓ R installed
[rstudio-server] ✓ RStudio Server installed
[rstudio-server] ✓ renv package installed
[rstudio-server] Waiting for push data...
[rstudio-server] ✓ Lockfile received
[rstudio-server] Running remote configure...
[rstudio-server] ✓ renv::restore() complete (47 packages)
[rstudio-server] ✓ ec2-user password set
[rstudio-server] Starting RStudio Server...
[rstudio-server] ✓ rstudio-server is running

RStudio Server is ready:
  URL:      http://54.12.34.56:8787
  Login:    ec2-user
  Password: my-secure-password
```

---

## Step 6: Log In to RStudio Server

Open the URL in your browser:

```
http://54.12.34.56:8787
```

Log in with:
- **Username:** `ec2-user`
- **Password:** `my-secure-password` (or whatever you passed as `--config password=`)

You will see the familiar RStudio IDE interface running on the remote instance.

---

## Step 7: Verify Your Packages

In the RStudio console:

```r
library(ggplot2)
library(dplyr)

# Verify renv project
renv::status()
# * The project is synchronized with the lockfile.

# Quick plot to confirm everything works
ggplot(mtcars, aes(wt, mpg)) + geom_point()
```

The packages that were in your local `renv.lock` are available on the remote instance exactly as they were locally.

---

## Step 8: Check Plugin Status

```bash
spawn plugin status rstudio-server --instance i-0abc123def456789
```

```
Plugin:   rstudio-server
State:    running
Service:  rstudio-server (active, running)
URL:      http://54.12.34.56:8787
Uptime:   12m
```

---

## Step 9: Stop and Restart RStudio Server

```bash
# Stop
spawn plugin stop rstudio-server --instance i-0abc123def456789

# Restart
spawn plugin start rstudio-server --instance i-0abc123def456789
```

Or directly via SSH:

```bash
ssh ec2-user@rstudio-demo.spore.host "sudo systemctl restart rstudio-server"
```

---

## Step 10: Remove the Plugin

```bash
spawn plugin uninstall rstudio-server --instance i-0abc123def456789
```

```
[rstudio-server] Stopping RStudio Server...
[rstudio-server] ✓ rstudio-server stopped
[rstudio-server] Plugin removed
```

---

## Configuration Reference

| Option | Default | Description |
|---|---|---|
| `renv_lockfile` | `renv.lock` | Path to your renv.lock file |
| `r_version` | auto-detected | Override R version (informational) |
| `rstudio_version` | `2024.12.1-563` | RStudio Server version to install |
| `port` | `8787` | Port RStudio Server listens on |
| `password` | `spore` | Password for the `ec2-user` system account |

---

## Tip: Use an Existing renv.lock Without Running R Locally

If you have a lockfile but do not have R installed locally (e.g., running from CI):

```bash
spawn plugin install github:spore-host/spore-host-plugin-rstudio/rstudio-server \
  --instance i-0abc123def456789 \
  --config renv_lockfile=path/to/renv.lock \
  --config r_version=4.3.2 \
  --config password=my-password
```

The `r_version` config is informational only (used for future version-pinning features) and does not affect which R is installed on the instance.

---

## Tip: Private Access via Tailscale

For security, install Tailscale first and access RStudio Server over the Tailnet — no public port 8787 required:

```bash
# Install Tailscale plugin
spawn plugin install github:spore-host/spore-host-plugin-tailscale/tailscale \
  --instance i-0abc123def456789 \
  --config auth_key=tskey-auth-...

# Install RStudio plugin
spawn plugin install github:spore-host/spore-host-plugin-rstudio/rstudio-server \
  --instance i-0abc123def456789 \
  --config password=my-secure-password
```

Access RStudio via the 100.x.x.x Tailscale IP:

```
http://100.x.x.x:8787
```

No security group changes needed. The port is only reachable over the private Tailnet.

---

## Troubleshooting

### RStudio Server won't start

```bash
ssh ec2-user@rstudio-demo.spore.host \
  "sudo journalctl -u rstudio-server -n 50 --no-pager"
```

Common causes:
- Port 8787 conflict (another process is using it)
- Insufficient RAM (try a larger instance type)

### Package restore failed

```bash
ssh ec2-user@rstudio-demo.spore.host \
  "Rscript -e 'renv::restore(lockfile=\"/home/ec2-user/project/renv.lock\", prompt=FALSE)'"
```

Some packages with compiled C/C++ extensions may need additional system libraries. Install them via `dnf install` and retry.

### Can't log in to RStudio

Ensure the password you set doesn't contain special characters that `chpasswd` might misinterpret. Stick to alphanumeric + `-_` characters.

---

## What You Learned

- The rstudio-server plugin uses a push flow to transfer your renv.lock to the instance before configuring
- RStudio Server uses PAM (system account) auth — `chpasswd` sets the `ec2-user` password
- renv::restore() ensures exact package versions match your local environment
- Combine with Tailscale to avoid exposing port 8787 publicly

---

## Next Steps

- [Tutorial 12: Tailscale Plugin](12-tailscale-plugin.md) — private networking for secure access
- [Tutorial 14: spore-sync Plugin](14-spore-sync-plugin.md) — sync your R scripts in real time
- [Tutorial 11: Advanced spawn](11-advanced-spawn.md) — run R sweeps across multiple instances

---

*[Back to Tutorial Index](README.md)*
