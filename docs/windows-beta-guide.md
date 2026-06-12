# spore.host Windows beta — step-by-step guide

A complete walkthrough for turning a **Windows 11 ISO** into a running, reachable
EC2 instance using spore.host's `spawn` tool: install spawn, sign in to **your
own AWS account**, build a custom Windows AMI from your ISO, launch it, and
connect by **Remote Desktop (RDP)** or a **PowerShell session over SSM**.

> **Who this is for:** someone new to spore.host. No prior `spawn` experience
> assumed. You *do* need an AWS account you can sign into, and a genuine Windows
> 11 Enterprise ISO + license (see Prerequisites).

> **Heads up on time + cost:** the one-time build takes **~45–75 min** end to
> end. spawn does two things in that window: (1) imports your ISO into a base
> AMI (~30–45 min), then (2) **pre-warms it** — launches a short-lived seed
> instance, lets Windows finish its one-time Sysprep first boot, and images that
> into a fast-boot "warm" AMI (see [§7](#7-the-build-pre-warms-your-ami)). The
> payoff: every instance you launch from the **warm** AMI is ready in **~4 min**,
> not ~30. spawn launches and **auto-terminates** the seed for you — but you pay
> normal EC2 + EBS charges for it during the build, for any instance you run, and
> a small ongoing storage charge for the two AMIs. **Terminate instances when
> you're done** ([§9](#9-clean-up--important--avoid-surprise-charges)).

---

## 0. Prerequisites

> **Note:** spawn works with the **AWS CLI** — it uses your AWS CLI credentials
> (you sign in with `aws login`) and the CLI's Session Manager support for the
> SSM connection paths. So a few steps below are `aws` commands; that's expected.

1. **A computer** running macOS, Linux, or Windows with a terminal.
2. **An AWS account you can sign into** (your own, via your org's access portal).
   `PowerUserAccess` or an admin-style permission set is simplest for a beta.
3. **The AWS CLI v2** installed: <https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html>
   (`aws --version` should print `aws-cli/2.x`).
   - *Optional:* the **Session Manager plugin** — only needed if you use the SSM
     connection paths (an interactive `spawn connect` shell, or `--rdp --via-ssm`).
     Plain `spawn connect <name> --rdp` to the public IP doesn't need it. Install
     it if/when you want those:
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

Verify (you want **v0.42.0 or newer** — earlier versions lack the pre-warmed
AMI build and the latest connect features):
```bash
spawn version
```

---

## 2. Sign in to your AWS account

spawn uses your normal AWS CLI credentials. Sign in with `aws login` — it opens a
browser, you log in to the AWS console as usual, and the CLI gets temporary
credentials (and auto-refreshes them; no long-lived keys):
```bash
aws login --profile sporehost
```
- `--profile sporehost` — names a profile you'll reuse (any name; `sporehost`
  here). Approve the request in the browser when it opens.
- On a remote/SSH box with no browser, use `aws login --remote --profile sporehost`
  — it prints a URL to open elsewhere and prompts for the code.

Point spawn (and the AWS CLI) at that profile and region for the rest of this
guide:
```bash
export AWS_PROFILE=sporehost
export AWS_REGION=us-east-1
```
(`us-east-1` is recommended for the beta — it's the simplest region for the
ISO-import build; other regions need extra VPC networking setup.)

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

This converts the ISO into an AMI using AWS EC2 Image Builder, then pre-warms it
(§7). spawn uploads the ISO, provisions the needed IAM roles + build
infrastructure automatically, runs the managed import, builds the warm AMI, and
cleans up the staged ISO and the warm-build seed afterward.

```bash
spawn image import \
  --iso "~/Downloads/Win11_Enterprise_25H2.iso" \
  --name win11-beta \
  --image-index 3 \
  --version 1.0.0
```
- `--image-index 3` — use the number `spawn image verify` reported.
- **No `--wait` needed.** Because pre-warming is on by default, the command
  blocks through the whole build (~45–75 min) and prints both AMI ids at the end.
  (Pre-warming needs the base AMI to exist first, so the command waits for you.)
- If you'd rather skip pre-warming and get just the base AMI back fast, add
  `--no-warm` — then the build runs async and you check progress with
  `spawn image status <build-arn>`. (You'll then pay the slow first boot on every
  launch — not recommended.)
- If you Ctrl-C during the build, it keeps running in AWS; the command tells you
  how to check on it. (A warm build interrupted mid-way still auto-terminates its
  seed instance.)

When it finishes you'll see **two** AMI ids:
```
Imported base AMI: ami-0123456789abcdef0
Warm AMI (fast boot, recommended): ami-0fedcba9876543210
Launch it with:
  spawn launch <name> --ami ami-0fedcba9876543210 --os windows --ttl 4h
```
Copy the **Warm AMI** id (the one spawn labels *recommended*) — that's the
fast-booting one you'll launch in Step 6. The base AMI is kept too (it's the
parent the warm one was built from).

---

## 6. Launch a Windows instance

Use the **Warm AMI** id from Step 5 (the one labelled *recommended*):
```bash
spawn launch win11-test \
  --ami ami-0fedcba9876543210 \
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

## 7. The build pre-warms your AMI (so launches are fast)

A freshly-imported Windows AMI runs Windows **Sysprep Specialize + OOBE**
("Getting ready" screen) on its first boot — normal Windows behavior, ~20–30
minutes, during which RDP/SSM aren't available. **You don't pay this on every
launch.** During the build (Step 5), spawn pays it **once** on your behalf:

1. It launches a short-lived **seed** instance from the imported base AMI.
2. It waits for that seed to finish its first boot (Administrator password
   available; spored + SSM agent installed).
3. It images the seed into a **warm AMI** and **terminates the seed**.

So the warm AMI you launch in Step 6 is already past Sysprep — it reaches
RDP/SSM-ready in **~4 minutes**, like a normal Windows boot. The slow ~30-min
first boot only happens if you launch the **base** AMI directly (or built with
`--no-warm`).

If you ever do launch the base AMI and want to watch the slow first boot, the
AWS console shows it: **EC2 → Instances → select it → Actions → Monitor and
troubleshoot → Get system log / Get instance screenshot**.

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

### B. PowerShell over SSM — a shell / one-off commands

Interactive PowerShell shell (no SSH keys or open ports needed; uses SSM):
```bash
spawn connect win11-test
```
Run a single command and exit:
```bash
spawn connect win11-test -- 'Get-ComputerInfo | Select-Object WindowsProductName, OsBuildNumber'
```

> PowerShell over SSM needs the instance to have registered with SSM. On the warm AMI
> that's ~4 min after launch; if `connect` can't reach it yet, wait a moment and
> retry. (On the base AMI it's only after the full ~30-min first boot — §7.)

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

The build leaves **two** custom AMIs — the base and the warm one — each with an
EBS snapshot that costs a little to store. List them and delete both when you no
longer need them:
```bash
spawn ami list                       # shows spawn-managed AMIs (base + warm)
spawn ami delete ami-0fedcba9876543210   # the warm AMI
spawn ami delete ami-0123456789abcdef0   # the base AMI
```
(`spawn ami delete` deregisters the AMI and removes its snapshot.)

To sign out of AWS at the end of the day:
```bash
aws logout --profile sporehost
```

---

## Quick reference

| Step | Command |
|------|---------|
| Sign in | `aws login --profile sporehost` |
| Verify ISO | `spawn image verify <iso>` |
| Build AMI (auto-warms) | `spawn image import --iso <iso> --name <n> --image-index <N>` |
| Launch (use the **warm** AMI id) | `spawn launch <name> --ami <warm-id> --os windows --ttl 2h --yes` |
| RDP | `spawn connect <name> --rdp` |
| RDP via SSM | `spawn connect <name> --rdp --via-ssm` |
| PowerShell (SSM) | `spawn connect <name>` |
| Terminate | `spawn terminate <name>` |
| List AMIs | `spawn ami list` |
| Delete AMI (do both: base + warm) | `spawn ami delete <ami-id>` |

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
