# spore.host Windows beta — step-by-step guide

A complete walkthrough for turning a **Windows 11 ISO** into a running, reachable
EC2 instance using spore.host's `spawn` tool: install spawn, sign in to **your
own AWS account**, build a custom Windows AMI from your ISO, launch it, and
connect by **Remote Desktop (RDP)** or **SSH-over-SSM**.

> **Who this is for:** someone new to spore.host. No prior `spawn` experience
> assumed. You *do* need an AWS account you can sign into, and a genuine Windows
> 11 Enterprise ISO + license (see Prerequisites).

> **Heads up on time + cost:** the one-time AMI build takes ~30–45 min, and the
> **first launch of a freshly-imported Windows AMI is slow** (~30 min) because
> Windows runs its full first-boot setup (Sysprep). This is expected — see
> [§7](#7-why-the-first-boot-is-slow). You pay normal EC2 + EBS charges for any
> instance you run, so **terminate instances when you're done**.

---

## 0. Prerequisites

1. **A computer** running macOS, Linux, or Windows with a terminal.
2. **An AWS account you can sign into** via your organization's AWS access portal
   (IAM Identity Center / SSO). You'll need permission to launch EC2 instances,
   create IAM roles, use EC2 Image Builder, and read/write S3 — `PowerUserAccess`
   or an admin-style permission set is simplest for a beta.
3. **The AWS CLI v2** installed: <https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html>
   (`aws --version` should print `aws-cli/2.x`). Also install the **Session
   Manager plugin** (needed for SSH-over-SSM and RDP-over-SSM):
   <https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html>
4. **An RDP client** (for the Remote Desktop step):
   - macOS: "Windows App" / "Microsoft Remote Desktop" from the App Store
   - Windows: built-in `mstsc`
   - Linux: `xfreerdp`
5. **A supported Windows 11 ISO + license (BYOL).** EC2 Image Builder accepts
   **Windows 11 Enterprise 23H2 / 24H2 / 25H2 (x64)**, downloaded from the
   **Microsoft 365 admin center** (the "business editions" ISO). It does **not**
   accept Evaluation ISOs, Media-Creation-Tool ISOs, or LTSC. You must hold a
   Microsoft license for what you run.

   You don't have to guess whether your ISO qualifies — `spawn image verify`
   (Step 4) checks it locally before you spend anything.

---

## 1. Install spawn

Pick one:

**macOS / Linux (Homebrew):**
```bash
brew install spore-host/tap/spawn
```

**Windows (Scoop):**
```powershell
scoop bucket add spore-host https://github.com/spore-host/scoop-bucket
scoop install spawn
```

**Or download a binary** from the latest release and put it on your `PATH`:
<https://github.com/spore-host/spawn/releases/latest>

Verify (you want **v0.40.0 or newer** — earlier versions don't have the Windows
connect features):
```bash
spawn version
```

---

## 2. Sign in to your AWS account (SSO)

spawn uses your normal AWS CLI credentials. The modern, recommended way is AWS
IAM Identity Center (SSO) — you log in through a browser, no long-lived keys.

**One-time setup** (`aws configure sso`):
```bash
aws configure sso
```
You'll be prompted for:
- **SSO session name** — anything memorable, e.g. `sporehost`.
- **SSO start URL** — your org's AWS access portal URL, e.g.
  `https://my-org.awsapps.com/start` (your IT/cloud admin has this).
- **SSO region** — the region your Identity Center lives in, e.g. `us-east-1`.
- **SSO registration scopes** — accept the default `sso:account:access`.

The browser opens; approve the request. Back in the terminal, pick your
**account** and **role** from the lists, then set:
- **Default client Region** — `us-east-1` (recommended for the beta; it avoids
  needing a NAT gateway during the AMI build — see [§7](#7-why-the-first-boot-is-slow)).
- **CLI default output format** — `json`.
- **Profile name** — e.g. `sporehost`.

**Each working session**, refresh your login (credentials expire):
```bash
aws sso login --profile sporehost
```

Tell spawn (and the AWS CLI) which profile to use for the rest of this guide:
```bash
export AWS_PROFILE=sporehost
export AWS_REGION=us-east-1
```

Confirm you're signed into the right account:
```bash
aws sts get-caller-identity
```

---

## 3. Put your Windows ISO somewhere spawn can reach it

You just need the ISO file on your local disk. `spawn image import` will upload
it to S3 for you (into a managed bucket it creates), so you do **not** need to
pre-create a bucket.

Note the local path, e.g. `~/Downloads/Win11_Enterprise_25H2.iso`.

---

## 4. Verify the ISO is acceptable (free, local, no AWS)

Before spending ~40 minutes on a build, check the ISO:
```bash
spawn image verify "~/Downloads/Win11_Enterprise_25H2.iso"
```
You'll get a table of the Windows editions inside it and a verdict. You want:
```
ACCEPTED: contains Windows 11 Enterprise (x64). Import with --image-index 3.
```
- **ACCEPTED** → note the `--image-index` number it tells you (the Enterprise
  edition's slot; often `3`).
- **REJECTED** → it tells you why (Evaluation, consumer Home/Pro, etc.). Get the
  Enterprise "business editions" ISO from the M365 admin center.

---

## 5. Build the AMI from your ISO

This converts the ISO into an AMI using AWS EC2 Image Builder. It uploads the
ISO, provisions the needed IAM roles + build infrastructure automatically, runs
the managed import, and cleans up the staged ISO afterward.

```bash
spawn image import \
  --iso "~/Downloads/Win11_Enterprise_25H2.iso" \
  --name win11-beta \
  --image-index 3 \
  --version 1.0.0 \
  --wait
```
- `--image-index 3` — use the number `spawn image verify` reported.
- `--wait` — block until the AMI is built (~30–45 min), then print the AMI id.
  Without `--wait` the command returns immediately and you check progress with
  `spawn image status <build-arn>`.
- If you Ctrl-C during `--wait`, the build keeps running in AWS; the command
  tells you how to check on it.

When it finishes you'll see something like:
```
Imported AMI: ami-0123456789abcdef0
```
Copy that AMI id.

---

## 6. Launch a Windows instance

```bash
spawn launch win11-test \
  --ami ami-0123456789abcdef0 \
  --os windows \
  --ttl 2h \
  --yes
```
What each part does:
- `win11-test` — a name for this instance (you'll use it to connect).
- `--os windows` — tells spawn to treat it as Windows (RDP/SSM connect, Windows
  defaults). spawn automatically:
  - picks a non-burstable instance type (**m7i.xlarge**) — Windows first boot is
    painfully slow on cheap "t" instances, so spawn refuses those;
  - opens a security group for **RDP (3389)** and **SSH (22)**;
  - waits until the Windows Administrator password is available before declaring
    the instance ready.
- `--ttl 2h` — **time-to-live**: the instance automatically terminates after 2
  hours so you can't forget and leave it running. Always set this.
- `--yes` — skip the interactive confirmation prompt.

> **Restrict who can reach RDP** (recommended): add
> `--allow-cidr <your-public-ip>/32` so only your network can reach 3389/22.
> Find your IP at <https://checkip.amazonaws.com>. Without it, the ports are open
> to the internet (spawn warns you).

spawn prints the instance id and, when ready, how to connect.

---

## 7. Why the first boot is slow

The very first launch of a freshly-imported Windows AMI runs Windows **Sysprep
Specialize + OOBE** ("Getting ready" screen) — this is normal Windows behavior
and takes ~20–30 minutes, even on a fast instance. During this time RDP/SSM
aren't available yet. Be patient on the **first** launch.

(A future spore.host improvement will produce a pre-warmed AMI so subsequent
launches boot quickly — but for this beta, expect the slow first boot.)

You can watch progress in the AWS console: **EC2 → Instances → select it →
Actions → Monitor and troubleshoot → Get system log / Get instance screenshot**.

---

## 8. Connect

You have two ways in. Use the instance name from Step 6 (`win11-test`).

### A. Remote Desktop (RDP) — the graphical Windows desktop

```bash
spawn connect win11-test --rdp
```
spawn fetches and **decrypts the Administrator password** (using the key it
launched the instance with), prints the connection details, and opens your RDP
client pointed at the instance. Log in as **Administrator** with the printed
password.

If the instance has no public IP (or you prefer not to expose RDP), tunnel it
over AWS Session Manager instead:
```bash
spawn connect win11-test --rdp --via-ssm
```
This opens an encrypted tunnel and points your RDP client at `localhost:13389`
— no inbound RDP from the internet needed. (Requires the Session Manager plugin
from Step 0.)

### B. SSH-over-SSM — a PowerShell session / one-off commands

Interactive PowerShell shell (no SSH keys or open ports needed; uses SSM):
```bash
spawn connect win11-test
```
Run a single command and exit:
```bash
spawn connect win11-test -- 'Get-ComputerInfo | Select-Object WindowsProductName, OsBuildNumber'
```

> SSH-over-SSM needs the instance to have finished first boot and registered
> with SSM (a few minutes after the desktop is up on the first boot). If
> `connect` can't reach it yet, wait and retry.

---

## 9. Clean up (important — avoid surprise charges)

When you're done, terminate the instance:
```bash
spawn terminate win11-test
```
The `--ttl` from Step 6 is a backstop that terminates it automatically, but
don't rely on it — terminate explicitly when finished.

To list anything still running:
```bash
spawn list
```

The custom AMI (and its EBS snapshot) persist until you remove them — they cost
a little to store. To delete the AMI when you no longer need it:
```bash
spawn ami delete ami-0123456789abcdef0
```

To sign out of AWS at the end of the day:
```bash
aws sso logout
```

---

## Quick reference

| Step | Command |
|------|---------|
| Sign in | `aws sso login --profile sporehost` |
| Verify ISO | `spawn image verify <iso>` |
| Build AMI | `spawn image import --iso <iso> --name <n> --image-index <N> --wait` |
| Launch | `spawn launch <name> --ami <id> --os windows --ttl 2h --yes` |
| RDP | `spawn connect <name> --rdp` |
| RDP via SSM | `spawn connect <name> --rdp --via-ssm` |
| PowerShell (SSM) | `spawn connect <name>` |
| Terminate | `spawn terminate <name>` |
| Delete AMI | `spawn ami delete <ami-id>` |

## When something goes wrong
- **`spawn image verify` says REJECTED** — wrong ISO; get the Enterprise
  business-editions ISO from the M365 admin center.
- **Build fails** — check CloudWatch Logs group `/aws/imagebuilder/<name>`.
- **`connect` can't reach the instance** — it may still be on first boot
  (§7); check the instance screenshot in the EC2 console and retry in a few min.
- **RDP/SSM commands fail with "session-manager-plugin not found"** — install
  the plugin (Step 0).
- **Anything else** — capture the exact command + output and send it back to the
  spore.host team.
