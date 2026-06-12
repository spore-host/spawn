# spore.host Windows beta — step-by-step guide

This guide takes you from a **Windows 11 ISO** to a running Windows machine in
the cloud you can log into, using spore.host's `spawn` tool. You'll install
spawn, sign in to **your own AWS account**, turn your ISO into a reusable machine
image, launch a Windows instance from it, connect to it, and shut it down.

> **Who this is for:** someone new to spore.host — no prior `spawn` experience
> assumed. You need an AWS account you can sign into and a genuine Windows 11
> Enterprise ISO + license (details in Prerequisites).

> **What to expect on time and money:**
> - The **first part is a one-time build** that takes **~45–75 minutes** mostly
>   unattended. You do it once per ISO; the result is a reusable image.
> - After that, **launching a machine takes ~4 minutes** and connecting is quick.
> - You pay normal AWS charges for any instance while it runs, plus a small
>   ongoing charge to store the image. **So: terminate instances when you're done**
>   (the last step shows how). spawn also sets an automatic time limit as a
>   backstop.

The steps below are in order — just follow them top to bottom.

---

## 0. Prerequisites

> **A note on the AWS CLI.** spawn builds on the AWS command-line tool: it reuses
> the credentials you sign in with there, and uses it for some connection
> methods. That's why a couple of steps below are `aws ...` commands rather than
> `spawn ...` — that's normal, not a detour.

You'll need:

1. **A computer** running macOS, Linux, or Windows with a terminal.
2. **An AWS account you can sign into** (your own, via your organization's access
   portal). A `PowerUserAccess` or admin-style permission set is simplest for a
   beta.
3. **The AWS CLI v2** — install it, then check `aws --version` prints
   `aws-cli/2.x`: <https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html>
4. **A Windows 11 Enterprise ISO + license (bring your own).** Download the
   "business editions" ISO from the **Microsoft 365 admin center** — that's the
   one that works. Consumer ISOs (from the Media Creation Tool), Evaluation ISOs,
   and LTSC are **not** accepted. You must hold a Microsoft license for what you
   run. (Step 4 checks your ISO for you before you spend anything, so you don't
   have to guess.)
5. **An RDP client** — only if you want the graphical Windows desktop (Step 7):
   - macOS: "Windows App" / "Microsoft Remote Desktop" from the App Store
   - Windows: built-in `mstsc`
   - Linux: `xfreerdp`

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

Then check the version — you want **v0.43.0 or newer** (that's where Windows
`--ssh` and the latest connect fixes landed):
```bash
spawn version
```

---

## 2. Sign in to your AWS account

Sign in with `aws login`. It opens a browser, you log into the AWS console as
usual, and the CLI receives temporary credentials and keeps them fresh — no
long-lived access keys to manage:
```bash
aws login --profile sporehost
```
- `--profile sporehost` names a reusable profile (any name works; this guide uses
  `sporehost`). Approve the request in the browser when it opens.
- On a remote machine with no browser (e.g. over SSH), use
  `aws login --remote --profile sporehost` — it prints a URL to open elsewhere
  and asks you to paste back the code.

Now tell spawn and the AWS CLI which profile and region to use for the rest of
this guide:
```bash
export AWS_PROFILE=sporehost
export AWS_REGION=us-east-1
```
Use **`us-east-1`** for the beta — it's the simplest region for the image build
(other regions need extra network setup).

Confirm it worked:
```bash
aws sts get-caller-identity
```
This should print your account number and the identity you signed in as.

---

## 3. Have your Windows ISO ready

Just have the ISO file on your local disk and note its path, e.g.
`~/Downloads/Win11_Enterprise_25H2.iso`. You don't need to upload it anywhere —
spawn handles that for you in Step 5.

---

## 4. Check your ISO is the right one

This reads your ISO locally and tells you whether it'll work — **free, no AWS, no
charges** — so you don't waste ~45 minutes on a build that would be rejected:
```bash
spawn image verify "~/Downloads/Win11_Enterprise_25H2.iso"
```
A Windows ISO often bundles several editions (Home, Pro, Enterprise…). spawn
lists them and gives a verdict:
```
ACCEPTED: contains Windows 11 Enterprise (x64). Import with --image-index 3.
```
- **ACCEPTED** → note the **`--image-index`** number it reports. That's which
  edition inside the ISO to use (Enterprise's slot — often `3`); you'll pass it in
  the next step.
- **REJECTED** → it explains why. Get the Enterprise "business editions" ISO from
  the Microsoft 365 admin center and try again.

---

## 5. Build your machine image (one time)

This is the long, mostly-unattended step. It turns your ISO into an **AMI** — an
Amazon Machine Image, the reusable template you launch instances from. spawn does
everything: uploads the ISO, sets up the AWS build resources, runs the import,
and (see below) produces a fast-booting image — then cleans up after itself.

```bash
spawn image import \
  --iso "~/Downloads/Win11_Enterprise_25H2.iso" \
  --name win11-beta \
  --image-index 3
```
- `--image-index 3` — the number `spawn image verify` reported in Step 4.
- The command **runs for ~45–75 minutes and blocks until it's done** — no extra
  flags needed. Leave it running. (If you Ctrl-C, the build keeps going in AWS
  and the command tells you how to check back on it.)

### Why you get *two* images

When it finishes, spawn prints **two** AMI ids:
```
Imported base AMI: ami-0123456789abcdef0
Warm AMI (fast boot, recommended): ami-0fedcba9876543210
Launch it with:
  spawn launch <name> --ami ami-0fedcba9876543210 --os windows --ttl 4h
```
A freshly-imported Windows image is slow the *first* time it boots — Windows runs
its one-time "Getting ready" setup (Sysprep), which takes ~20–30 minutes. To
spare you that on every launch, spawn does it **once** during this build: it
quietly boots a throwaway instance from the base image, lets Windows finish
setup, snapshots the result into a second **"warm" image**, and discards the
throwaway. (That extra instance is billed only for those few minutes of the
build, and spawn shuts it down automatically.)

**Copy the Warm AMI id** (the one marked *recommended*) — that's the
fast-booting image you'll launch in the next step. The base image is kept too, as
the warm one's parent; you'll delete both at the end.

> Prefer just the base image and willing to wait through the slow first boot every
> time? Add `--no-warm` to skip the warm step (not recommended for the beta).

---

## 6. Launch a Windows instance

Launch from the **Warm AMI** id you copied in Step 5:
```bash
spawn launch win11-test \
  --ami ami-0fedcba9876543210 \
  --os windows \
  --ttl 2h \
  --yes
```
- `win11-test` — the name you'll use to connect to and manage this instance.
- `--ami ...` — the warm image from Step 5.
- `--os windows` — tells spawn this is Windows, so it picks Windows-appropriate
  defaults: a non-burstable instance type (`m7i.xlarge` — cheap "t" instances
  make Windows painfully slow, so spawn won't use them), opens the ports for
  Remote Desktop (3389) and SSH (22), and waits for the Windows Administrator
  password to be ready before calling the instance ready.
- `--ttl 2h` — **time-to-live**: the instance terminates itself after 2 hours so
  a forgotten machine can't run up a bill. Always set one.
- `--yes` — skip the confirmation prompt.

> **Lock down who can reach it (recommended):** add
> `--allow-cidr <your-public-ip>/32` so only your network can reach the RDP/SSH
> ports. Find your IP at <https://checkip.amazonaws.com>. Without it the ports are
> open to the internet (spawn warns you when that happens).

Because you launched the *warm* image, the instance is ready to connect in
**~4 minutes**. spawn prints the instance id and how to connect.

---

## 7. Connect

Three ways in — pick whichever suits you. All use the instance **name** from
Step 6 (`win11-test`).

### A. Remote Desktop (RDP) — the full graphical Windows desktop

```bash
spawn connect win11-test --rdp
```
spawn fetches and **decrypts the Administrator password** for you, prints the
connection details, and opens your RDP client pointed at the instance. Log in as
**Administrator** with the printed password. (Needs an RDP client from
Prerequisites.)

Don't want to expose Remote Desktop to the internet (or the instance has no
public IP)? Tunnel it privately instead:
```bash
spawn connect win11-test --rdp --via-ssm
```
This opens an encrypted tunnel through AWS and points your RDP client at
`localhost:13389` — nothing inbound from the internet. This route needs the AWS
**Session Manager plugin** installed (one-time):
<https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html>

### B. A command-line session (no extra ports, no RDP client)

Open an interactive PowerShell session routed privately through AWS:
```bash
spawn connect win11-test
```
Or run one command and exit:
```bash
spawn connect win11-test -- 'Get-ComputerInfo | Select-Object WindowsProductName, OsBuildNumber'
```
This route also uses AWS Session Manager, so it needs the **Session Manager
plugin** (same link as above). The instance must have finished registering with
AWS first — on the warm image that's ~4 min after launch; if `connect` can't
reach it yet, wait a moment and retry.

### C. Plain SSH (same as connecting to a Linux box)

The Windows image runs an SSH server and trusts spawn's launch key, so you can
SSH straight in — no Session Manager plugin needed. You log in as Administrator
and land at a PowerShell prompt:
```bash
spawn connect win11-test --ssh
```
Or run one command and exit (it runs in PowerShell):
```bash
spawn connect win11-test --ssh -- 'Get-ComputerInfo | Select-Object WindowsProductName'
```
This connects over the public internet on port 22, so the instance's firewall
must allow SSH from you — spawn opens it by default, and `--allow-cidr` at launch
(Step 6) restricts it to your network. You also need spawn's launch key on this
machine (it's there automatically if you launched from here).

---

## 8. Clean up — important, avoid surprise charges

When you're done, terminate the instance:
```bash
spawn terminate win11-test
```
The `--ttl` from Step 6 will terminate it eventually as a backstop, but don't
rely on that — shut it down explicitly when you finish.

Check nothing is still running:
```bash
spawn list
```

Finally, the build in Step 5 left **two images** (the base and the warm one),
each with a stored snapshot that costs a little to keep. When you no longer need
them, delete both:
```bash
spawn ami list                            # shows your spawn-managed images
spawn ami delete ami-0fedcba9876543210    # the warm image
spawn ami delete ami-0123456789abcdef0    # the base image
```

To sign out of AWS at the end of the day:
```bash
aws logout --profile sporehost
```

---

## Quick reference

| Task | Command |
|------|---------|
| Sign in | `aws login --profile sporehost` |
| Check an ISO | `spawn image verify <iso>` |
| Build the image (one time) | `spawn image import --iso <iso> --name <n> --image-index <N>` |
| Launch (use the **warm** image id) | `spawn launch <name> --ami <warm-id> --os windows --ttl 2h --yes` |
| Connect — Remote Desktop | `spawn connect <name> --rdp` |
| Connect — RDP, private tunnel | `spawn connect <name> --rdp --via-ssm` |
| Connect — command-line (SSM) | `spawn connect <name>` |
| Connect — SSH | `spawn connect <name> --ssh` |
| Terminate the instance | `spawn terminate <name>` |
| List your images | `spawn ami list` |
| Delete an image (do both) | `spawn ami delete <ami-id>` |
| Sign out | `aws logout --profile sporehost` |

---

## When something goes wrong

- **`spawn image verify` says REJECTED** — wrong ISO. Get the Enterprise
  "business editions" ISO from the Microsoft 365 admin center.
- **The build fails** — check the AWS CloudWatch Logs group
  `/aws/imagebuilder/<name>` (the `--name` you passed).
- **`connect` can't reach the instance** — it may still be starting up; wait a
  minute and retry. You can watch a Windows instance boot in the AWS console:
  **EC2 → Instances → select it → Actions → Monitor and troubleshoot → Get
  instance screenshot**.
- **A connect command fails with "session-manager-plugin not found"** — install
  the Session Manager plugin (linked in Step 7A), or use `--ssh` instead, which
  doesn't need it.
- **Anything else** — capture the exact command and its output, and send it to
  the spore.host team.
