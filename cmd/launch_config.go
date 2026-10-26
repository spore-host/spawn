package cmd

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/input"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/sporedinstall"
	"github.com/spore-host/spawn/pkg/sshkey"
	"github.com/spore-host/spawn/pkg/storage"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
	"github.com/spore-host/spawn/pkg/userdata"
)

// parseAttachVolumes turns repeated --attach-volume values of the form
// "snap-xxx:/mount/point[:ro|:rw]" into AttachVolumeSpec. The mount point must
// be an absolute path; the optional trailing :ro / :rw sets read-only (default
// read-write). Mount paths don't contain ':' in practice, so we split on it (#144).
func parseAttachVolumes(raw []string) ([]aws.AttachVolumeSpec, error) {
	specs := make([]aws.AttachVolumeSpec, 0, len(raw))
	for _, r := range raw {
		parts := strings.Split(r, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("invalid --attach-volume %q: expected snap-xxx:/mount/point[:ro]", r)
		}
		snap := strings.TrimSpace(parts[0])
		mount := strings.TrimSpace(parts[1])
		readOnly := false
		if len(parts) == 3 {
			switch strings.ToLower(strings.TrimSpace(parts[2])) {
			case "ro":
				readOnly = true
			case "rw":
				readOnly = false
			default:
				return nil, fmt.Errorf("invalid --attach-volume %q: mode must be 'ro' or 'rw', got %q", r, parts[2])
			}
		}
		if !strings.HasPrefix(snap, "snap-") {
			return nil, fmt.Errorf("invalid --attach-volume %q: snapshot must look like snap-xxxx", r)
		}
		if !strings.HasPrefix(mount, "/") {
			return nil, fmt.Errorf("invalid --attach-volume %q: mount point must be an absolute path", r)
		}
		specs = append(specs, aws.AttachVolumeSpec{
			SnapshotID: snap,
			MountPoint: mount,
			ReadOnly:   readOnly,
		})
	}
	return specs, nil
}

// attachedVolumesUserData maps the launch config's attach-volume specs to the
// storage user-data's mount list, assigning each the same EC2 device name the
// block-device mapping used (aws.AttachDeviceName), so the mount resolves the
// right device (#144).
func attachedVolumesUserData(specs []aws.AttachVolumeSpec) []userdata.AttachedVolume {
	return userdata.AttachedVolumesFrom(specs)
}

func getEFSMountOptions() (string, error) {
	// Custom mount options override profile
	if efsMountOptions != "" {
		opts, err := storage.ParseCustomOptions(efsMountOptions)
		if err != nil {
			return "", fmt.Errorf("failed to parse custom mount options: %w", err)
		}
		return opts.ToMountString(), nil
	}

	// Validate profile
	if efsProfile != "" {
		if err := storage.ValidateProfile(efsProfile); err != nil {
			return "", err
		}
	}

	// Get mount options from profile
	opts, err := storage.GetEFSProfile(storage.EFSProfile(efsProfile))
	if err != nil {
		return "", fmt.Errorf("failed to get EFS profile: %w", err)
	}

	return opts.ToMountString(), nil
}

func buildLaunchConfig(truffleInput *input.TruffleInput) (*aws.LaunchConfig, error) {
	config := &aws.LaunchConfig{
		Tags: make(map[string]string),
	}

	if err := validateOnIdle(onIdle); err != nil {
		return nil, err
	}
	// A bad --config file fails here, before any AWS work, on every path that
	// builds a config (single, sweep, pipe and --dry-run) (#1116).
	if _, err := collectPluginDeclarations(); err != nil {
		return nil, err
	}

	// From truffle input
	if truffleInput != nil {
		config.InstanceType = truffleInput.InstanceType
		config.Region = truffleInput.Region
		config.AvailabilityZone = truffleInput.AvailabilityZone

		if truffleInput.Spot {
			config.Spot = true
			if truffleInput.SpotPrice > 0 {
				config.SpotMaxPrice = fmt.Sprintf("%.4f", truffleInput.SpotPrice)
			}
		}
		// Carry the reservation id from truffle input (#216). Previously dropped —
		// only type/region/AZ/spot were copied, so a piped reservation never
		// reached RunInstances (same silently-dropped-field class as lagotto#19).
		if truffleInput.ReservationID != "" {
			config.ReservationID = truffleInput.ReservationID
		}
	}

	// Override with flags
	if instanceType != "" {
		config.InstanceType = instanceType
	}
	if region != "" {
		config.Region = region
	}
	if az != "" {
		config.AvailabilityZone = az
	}
	// "auto" is an explicit synonym for "auto-detect" (empty); normalize so all
	// downstream AMI gates auto-detect (#342).
	if ami != "" && !strings.EqualFold(ami, "auto") {
		config.AMI = ami
	}
	if launchVolumeSize > 0 {
		config.RootVolumeSizeGiB = launchVolumeSize
	}
	if len(attachVolumes) > 0 {
		specs, err := parseAttachVolumes(attachVolumes)
		if err != nil {
			return nil, err
		}
		config.AttachVolumes = specs
	}
	if keyPair != "" {
		config.KeyName = keyPair
	}
	// Network (#667). Both of these were parsed into globals and then never
	// copied onto the config: the only assignments lived on the batch-queue
	// path, so an ordinary launch silently used the VPC's default security group
	// and an arbitrary subnet. Nothing warned, and the symptom surfaced much
	// later as a hung `hard` NFS mount or a refused SSH.
	if len(sgIDs) > 0 {
		config.SecurityGroupIDs = sgIDs
	}
	if subnetID != "" {
		config.SubnetID = subnetID
	}
	// --vpc (#673). Previously parsed and read by NOTHING: there was no VPCID
	// field at all, and every consumer called GetDefaultVPC unconditionally, so
	// this flag was accepted and the instance launched in the default VPC.
	if vpcID != "" {
		config.VPCID = vpcID
	}
	// --efa without --mpi. The only assignment of EFAEnabled lived in
	// resolveMPIPlacement, so a plain `spawn launch --efa` passed the EFA
	// pre-flight and then launched with an ordinary ENA interface: no EFA device,
	// fi_info -p efa empty, nothing warned. Heterogeneous EFA pairs (different
	// instance types, so no MPI job array) depend on this path.
	if efaEnabled {
		config.EFAEnabled = true
	}
	// --efa-interfaces (#661): 0 is resolved to the type's MaximumEfaInterfaces
	// by resolveEFAInterfaces, which also rejects a count above it.
	if efaInterfaces < 0 {
		return nil, fmt.Errorf("--efa-interfaces must be 0 (every EFA the type supports) or a positive count, got %d", efaInterfaces)
	}
	if efaInterfaces != 0 && !efaEnabled {
		return nil, fmt.Errorf("--efa-interfaces requires --efa")
	}
	config.EFAInterfaces = efaInterfaces
	// --placement-group without --mpi: same gap. The flag was applied only in
	// resolveMPIPlacement, which returns early when --mpi is off, so an explicit
	// cluster placement group was silently ignored on a single launch.
	if mpiPlacementGroup != "" {
		config.PlacementGroup = mpiPlacementGroup
	}
	if spot {
		config.Spot = true
	}
	// Launch into a Capacity Reservation / Capacity Block (#216). The flag
	// overrides any truffle-supplied id.
	if reservationID != "" {
		config.ReservationID = reservationID
	}
	if capacityBlock {
		config.CapacityBlock = true
	}
	// A Capacity Block is consumed via MarketType=capacity-block, which is
	// mutually exclusive with Spot's market options — reject the combination
	// rather than silently dropping one.
	if config.CapacityBlock && config.Spot {
		return nil, fmt.Errorf("--capacity-block and --spot are mutually exclusive (a Capacity Block is not a Spot purchase)")
	}
	// --capacity-block only means something with a reservation to target.
	if config.CapacityBlock && config.ReservationID == "" {
		return nil, fmt.Errorf("--capacity-block requires --reservation-id (the Capacity Block id to launch into)")
	}
	if hibernate {
		config.Hibernate = true
	}
	if nestedVirt {
		config.NestedVirtualization = true
	}
	if ttl != "" {
		config.TTL = ttl
		// A --ttl longer than the reaper's max-age ceiling is silently
		// ineffective: the ceiling applies even inside a live deadline, because
		// spawn:ttl-deadline is writable from the instance and so cannot be the
		// only bound (#76). Say so here rather than let the user discover it when
		// the instance disappears early.
		//
		// Parsed with the shared parser (#990). This used to be
		// time.ParseDuration, whose failure on "1d" only skipped the warning —
		// and the launch then went ahead with spawn:ttl=1d and no deadline.
		d, err := ttlpkg.ParseTTL(ttl)
		if err != nil {
			return nil, fmt.Errorf("invalid --ttl %q: %w (use a duration such as 8h, 90m, 1d or 2d12h)", ttl, err)
		}
		if w := ttlpkg.CeilingWarning(d); w != "" {
			fmt.Fprintln(os.Stderr, w)
		}
	}
	// --name implies DNS registration; --dns overrides the DNS portion only.
	if dnsName == "" && name != "" {
		dnsName = name
	} else if name == "" && dnsName != "" {
		name = dnsName
	}
	// The spawn:dns-name tag (written from config.DNSName) is what makes spored
	// register DNS, so --no-dns / dns.enabled: false must withhold it. They used to
	// gate only the CLI's own registration attempt, while spored registered the
	// name anyway: "skip DNS registration entirely" skipped nothing (#872).
	if dnsName != "" && dnsRegistrationEnabled() {
		config.DNSName = dnsName
	}
	if slackWorkspaceID != "" {
		config.SlackWorkspaceID = slackWorkspaceID
		// The spore-bot Lambda Function URL — hard-coded for hosted spore.host;
		// can be overridden via SPORE_NOTIFY_URL env var for self-hosted deployments.
		notifyURL := spawnconfig.GetNotifyURL()
		config.NotifyURL = notifyURL
		config.NotifyCommand = "/spore" // routes notifications to spore-bot workspace config
	}
	if notifyPlatform != "" {
		config.NotifyPlatform = notifyPlatform // slack (default) / teams / discord (#2)
	}
	if activePorts != "" {
		config.ActivePortsRaw = activePorts
	}
	if activeProcesses != "" {
		config.ActiveProcessesRaw = activeProcesses
	}
	if idleTimeout != "" {
		config.IdleTimeout = idleTimeout
	}
	// Idle action: canonical --on-idle enum, with the deprecated boolean
	// --hibernate-on-idle folded in (#316). --on-idle wins if both are given.
	if onIdle == "hibernate" || hibernateOnIdle {
		config.HibernateOnIdle = true
	}
	if preStop != "" {
		config.PreStop = preStop
	}
	if preStopTimeout != "" {
		config.PreStopTimeout = preStopTimeout
	}
	if spotWebhookURL != "" {
		config.SpotInterruptionWebhookURL = spotWebhookURL
		config.WebhookCorrelation = webhookCorrelation
		config.WebhookTimeout = webhookTimeout
	}
	if completionWebhookURL != "" {
		config.CompletionWebhookURL = completionWebhookURL
		config.WebhookCorrelation = webhookCorrelation
		config.WebhookTimeout = webhookTimeout
	}
	if onComplete != "" {
		config.OnComplete = onComplete
	}
	// --on-idle hibernate / --hibernate-on-idle / --on-complete hibernate imply
	// --hibernate (#949). EC2 hibernates only an instance launched configured for
	// it; without this the idle or completion hibernate silently became a stop
	// (RAM lost) and a persistent Spot request was interrupted by stop. Setting it
	// here, not only in pkg/aws, is what makes the hibernation pre-flight, the
	// dry-run plan and the cost estimate see it.
	if config.HibernationRequested() {
		config.Hibernate = true
	}
	if completionFile != "" {
		config.CompletionFile = completionFile
	}
	if completionDelay != "" {
		config.CompletionDelay = completionDelay
	}
	if sessionTimeout != "" {
		config.SessionTimeout = sessionTimeout
	}
	if name != "" {
		config.Name = name
	}
	if efsID != "" {
		config.EFSID = efsID
	}
	if efsMountPoint != "" {
		config.EFSMountPoint = efsMountPoint
	}

	// FSx Lustre flags
	config.FSxLustreCreate = fsxCreate
	config.FSxLifecycle = fsxLifecycle
	config.FSxTTL = fsxTTL
	if fsxID != "" {
		config.FSxLustreID = fsxID
	}
	if fsxRecall != "" {
		config.FSxLustreRecall = fsxRecall
	}
	if fsxStorageCapacity > 0 {
		config.FSxStorageCapacity = fsxStorageCapacity
	}
	if fsxS3Bucket != "" {
		config.FSxS3Bucket = fsxS3Bucket
	}
	if fsxImportPath != "" {
		config.FSxImportPath = fsxImportPath
	}
	if fsxExportPath != "" {
		config.FSxExportPath = fsxExportPath
	}
	if fsxMountPoint != "" {
		config.FSxMountPoint = fsxMountPoint
	}

	if costLimit > 0 {
		config.CostLimit = costLimit
	}
	if command != "" {
		config.JobArrayCommand = command
	}

	// Validate FSx flags
	if fsxCreate && fsxID != "" {
		return nil, fmt.Errorf("cannot use --fsx-create and --fsx-id together")
	}
	if fsxCreate && fsxRecall != "" {
		return nil, fmt.Errorf("cannot use --fsx-create and --fsx-recall together")
	}
	if fsxID != "" && fsxRecall != "" {
		return nil, fmt.Errorf("cannot use --fsx-id and --fsx-recall together")
	}
	if fsxCreate && fsxS3Bucket == "" {
		return nil, fmt.Errorf("--fsx-create requires --fsx-s3-bucket")
	}

	// Lifecycle contract (#193): an auto-created FSx is expensive and holds the
	// only copy of results, so its lifetime must be stated explicitly — never
	// inferred or defaulted. Fail closed if --fsx-create lacks a lifecycle, and
	// require a TTL for durable so no death-clock-less filesystem can exist.
	if fsxCreate {
		switch fsxLifecycle {
		case "ephemeral":
			if fsxTTL != "" {
				return nil, fmt.Errorf("--fsx-ttl is only valid with --fsx-lifecycle=durable (an ephemeral FSx needs no TTL: it is reclaimed asynchronously once no instance references it)")
			}
		case "durable":
			if fsxTTL == "" {
				return nil, fmt.Errorf("--fsx-lifecycle=durable requires --fsx-ttl (e.g. 7d) — a durable FSx must have a death clock so it can't bill indefinitely")
			}
			if _, err := parseDuration(fsxTTL); err != nil {
				return nil, fmt.Errorf("invalid --fsx-ttl %q: %w", fsxTTL, err)
			}
		case "":
			return nil, fmt.Errorf("--fsx-create requires --fsx-lifecycle: 'ephemeral' (reclaimed asynchronously once the instance is gone) or 'durable' (persists; needs --fsx-ttl). An FSx costs money and holds your results — choose its lifetime explicitly")
		default:
			return nil, fmt.Errorf("invalid --fsx-lifecycle %q: must be 'ephemeral' or 'durable'", fsxLifecycle)
		}
	}
	if !fsxCreate && (fsxLifecycle != "" || fsxTTL != "") {
		return nil, fmt.Errorf("--fsx-lifecycle/--fsx-ttl only apply with --fsx-create")
	}

	// Validate storage capacity (must be 1200, 2400, or multiples of 2400)
	if fsxCreate && fsxStorageCapacity > 0 {
		if fsxStorageCapacity < 1200 {
			return nil, fmt.Errorf("minimum FSx storage capacity is 1200 GB")
		}
		if fsxStorageCapacity != 1200 && fsxStorageCapacity != 2400 && (fsxStorageCapacity-2400)%2400 != 0 {
			return nil, fmt.Errorf("invalid FSx storage capacity: must be 1200, 2400, or increments of 2400")
		}
	}

	// Every lifecycle duration, including those from ~/.spawn/config.yaml
	// defaults, must parse with the shared parser before anything touches AWS:
	// a value spored cannot read is silently not enforced (#990).
	if err := config.ValidateDurations(); err != nil {
		return nil, err
	}

	return config, nil
}

// validateLaunchDurationFlags checks the duration flags of `spawn launch` with
// the shared parser (#990). runLaunch calls it first, before its first AWS
// call and before any sweep, batch-queue or wizard branch, so `--ttl 1w` fails
// in the shell on every launch path. buildLaunchConfig then re-checks the
// assembled config, which also covers ~/.spawn/config.yaml defaults.
func validateLaunchDurationFlags() error {
	return aws.LaunchConfig{
		TTL:             ttl,
		IdleTimeout:     idleTimeout,
		PreStopTimeout:  preStopTimeout,
		CompletionDelay: completionDelay,
		WebhookTimeout:  webhookTimeout,
		FSxTTL:          fsxTTL,
	}.ValidateDurations()
}

// encodeUserData gzip-compresses the script and base64-encodes the result.
// cloud-init on Amazon Linux 2023 and Ubuntu supports gzip+base64 user-data,
// which keeps the payload well under EC2's 16 KB limit even when combining
// spored bootstrap + MPI + FSx mount scripts (fixes #304).
// decodeUserData is the inverse of encodeUserData: base64-decode, then gunzip if
// the payload is actually gzipped (spawn#671).
//
// It exists because the job-array path has to APPEND to an already-encoded base
// script, and appending to the compressed bytes is what shipped
// gzip(gzip(bootstrap) + mpiScript) — cloud-init unwraps one layer, finds binary,
// and silently skips the whole user-data, so no member ever bootstrapped.
//
// The gzip check is a magic-number test rather than an assumption, because
// encodeUserDataForOS deliberately does NOT gzip for Windows (EC2Launch cannot
// decompress). A plain-base64 payload therefore round-trips through here too,
// instead of erroring on a missing gzip header.
func decodeUserData(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode user-data: %w", err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		return string(raw), nil // not gzipped (Windows path)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("gunzip user-data: %w", err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("read gunzipped user-data: %w", err)
	}
	return string(out), nil
}

func encodeUserData(script string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(script))
	_ = gz.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// encodeUserDataForOS picks the right encoding for the target OS. Windows
// EC2Launch does NOT gzip-decompress user-data (only cloud-init does), so a
// Windows <powershell> script must be plain base64, not gzip+base64.
func encodeUserDataForOS(script, targetOS string) string {
	if targetOS == "windows" {
		return base64.StdEncoding.EncodeToString([]byte(script))
	}
	return encodeUserData(script)
}

// spawnPublicKeyForUserData returns the authorized_keys-format public key to
// install on the instance, matching the keypair registered with EC2. It resolves
// the private key for keyName via the shared resolver and reads its ".pub"; if
// that's unavailable (e.g. a user-supplied/legacy key only in ~/.ssh), it falls
// back to the personal public key for back-compat.
func spawnPublicKeyForUserData(plat *platform.Platform, keyName string) ([]byte, error) {
	// When a key pair is named, install THAT key's public half for the local
	// user — deriving it from the private key if no .pub file exists. Never
	// silently fall back to a different key (e.g. ~/.ssh/id_rsa.pub): that leaves
	// the local-matching user trusting a key the operator isn't connecting with,
	// so login fails with Permission denied (#349).
	if keyName != "" {
		pub, err := sshkey.PublicKeyForName(plat.HomeDir, keyName)
		if err != nil {
			return nil, fmt.Errorf("resolve public key for key pair %q: %w", keyName, err)
		}
		return pub, nil
	}
	pub, err := plat.ReadPublicKey()
	if err != nil {
		return nil, fmt.Errorf("failed to read SSH public key: %w", err)
	}
	return pub, nil
}

// buildWindowsUserData emits the EC2Launch <powershell> bootstrap for a Windows
// instance (#55/#77). It (1) enables OpenSSH and trusts the spawn public key so
// `spawn connect` can SSH-over-SSM in addition to the Administrator-password/RDP
// path, and (2) installs spored as a Windows Service from the regional S3 bucket
// — the same buckets/binary the Linux bootstrap uses — so the instance enforces
// TTL/idle/completion in-instance just like Linux.
//
// It starts no workload: no --command, MPI, task runner or custom user data.
// If it ever does, gate that start on `spored is-spot-replacement` exiting 0, as
// the Linux bootstrap does (#1007), or a Spot replacement will re-run it.
func buildWindowsUserData(authorizedKey string) (string, error) {
	// authorizedKey is an authorized_keys line (e.g. "ssh-rsa AAAA... spawn").
	// Guard against breaking out of the PowerShell here-string.
	if strings.Contains(authorizedKey, "\"@") || strings.Contains(authorizedKey, "@\"") {
		return "", fmt.Errorf("invalid public key content for Windows user-data")
	}
	key := strings.TrimSpace(authorizedKey)

	// EC2Launch v2 runs the <powershell> block on first boot. We:
	//  1. install + enable the OpenSSH Server optional feature,
	//  2. write the spawn public key to administrators_authorized_keys (the file
	//     Windows OpenSSH uses for members of the Administrators group) with the
	//     ACL it requires (Administrators + SYSTEM only),
	//  3. set the default shell to PowerShell for nicer interactive sessions,
	//  4. download spored.exe from the regional spawn-binaries S3 bucket and
	//     install it as an auto-start Windows Service (spored's own subcommand
	//     sets recovery actions). The instance role carries S3 read access, the
	//     same as Linux. Mirrors install-spored.ps1.
	script := fmt.Sprintf(`<powershell>
$ErrorActionPreference = "Continue"
try {
  Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 -ErrorAction SilentlyContinue
  Set-Service -Name sshd -StartupType Automatic
  Start-Service sshd

  # Open inbound TCP 22 for ALL firewall profiles. The OpenSSH feature only
  # creates a Private-profile allow rule, but an EC2 instance's network is
  # classified Public, so SSH-to-the-public-IP (spawn connect --ssh) is blocked
  # by Windows Firewall even though sshd listens and the EC2 security group
  # allows it. (Confirmed live: RDP worked, SSH didn't, until this rule.)
  if (-not (Get-NetFirewallRule -Name spawn-sshd-22 -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name spawn-sshd-22 -DisplayName "spawn OpenSSH 22 (all profiles)" `+"`"+`
      -Enabled True -Direction Inbound -Protocol TCP -LocalPort 22 -Action Allow -Profile Any `+"`"+`
      -ErrorAction SilentlyContinue | Out-Null
  }

  $admins = "C:\ProgramData\ssh\administrators_authorized_keys"
  Set-Content -Path $admins -Value "%s" -Encoding ascii
  icacls $admins /inheritance:r | Out-Null
  icacls $admins /grant "Administrators:F" /grant "SYSTEM:F" | Out-Null

  New-ItemProperty -Path "HKLM:\SOFTWARE\OpenSSH" -Name DefaultShell `+"`"+`
    -Value "C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe" `+"`"+`
    -PropertyType String -Force | Out-Null
} catch {
  Write-Output "spawn windows ssh bootstrap warning: $_"
}

# Ensure the SSM agent is installed, set to auto-start, and running (#95).
# Stock Windows *Server* AMIs ship it running, but an imported Windows 11
# *client* AMI (spawn image import) bakes the agent without it auto-starting
# after Sysprep, so SSM never registers and SSH-over-SSM / RDP-over-SSM fail.
# Don't assume the AMI did it — same posture as the AWS CLI install below.
try {
  $svc = Get-Service -Name AmazonSSMAgent -ErrorAction SilentlyContinue
  if (-not $svc) {
    $msi = "$env:TEMP\SSMAgent_latest.exe"
    Invoke-WebRequest -Uri 'https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/windows_amd64/AmazonSSMAgentSetup.exe' -OutFile $msi -UseBasicParsing
    Start-Process $msi -ArgumentList '/S' -Wait
  }
  Set-Service -Name AmazonSSMAgent -StartupType Automatic -ErrorAction SilentlyContinue
  Restart-Service -Name AmazonSSMAgent -ErrorAction SilentlyContinue
} catch {
  Write-Output "spawn ssm-agent bootstrap warning: $_"
}

# Install spored as a Windows Service from the regional S3 bucket (#77).
try {
  $token = Invoke-RestMethod -Method Put -Uri 'http://169.254.169.254/latest/api/token' -Headers @{'X-aws-ec2-metadata-token-ttl-seconds'='21600'} -TimeoutSec 5
  $region = Invoke-RestMethod -Uri 'http://169.254.169.254/latest/meta-data/placement/region' -Headers @{'X-aws-ec2-metadata-token'=$token} -TimeoutSec 5
  if (-not $region) { $region = 'us-east-1' }

  # The stock Windows Server AMI has no AWS CLI (unlike AL2023), so install it
  # before pulling spored from S3. aws.exe lands in Program Files; call it by
  # full path since the current session PATH won't yet include it.
  $aws = "$env:ProgramFiles\Amazon\AWSCLIV2\aws.exe"
  if (-not (Test-Path $aws)) {
    $msi = "$env:TEMP\AWSCLIV2.msi"
    Invoke-WebRequest -Uri 'https://awscli.amazonaws.com/AWSCLIV2.msi' -OutFile $msi -UseBasicParsing
    Start-Process msiexec.exe -ArgumentList @('/i', $msi, '/qn') -Wait
  }

  # Verified install (#879, GHSA-q59f-6gc3-qj2p): binary, .sha256 and .sig from
  # the first source that passes BOTH checks; nothing is installed otherwise.
  # Generated by pkg/sporedinstall.WindowsInstallScript, which derives the key
  # from the same SigningKeyPEM the Linux bootstrap uses.
__SPORED_WINDOWS_VERIFIED_INSTALL__
  if ($sporedInstalled) {
    & $exe service install $exe
    & $exe service start
  }
} catch {
  Write-Output "spawn spored install warning: $_"
}
</powershell>
<persist>false</persist>`, key)

	// Spliced in AFTER Sprintf, deliberately: the generated PowerShell is not a
	// format string, and a percent sign in it (PowerShell's ForEach-Object alias) would be
	// read as a verb.
	install, err := sporedinstall.WindowsInstallScript(sporedinstall.SigningKeyPEM)
	if err != nil {
		return "", fmt.Errorf("build the Windows spored install: %w", err)
	}
	if !strings.Contains(script, "__SPORED_WINDOWS_VERIFIED_INSTALL__") {
		return "", fmt.Errorf("the Windows bootstrap template lost its verified-install placeholder")
	}
	script = strings.Replace(script, "__SPORED_WINDOWS_VERIFIED_INSTALL__", install, 1)

	return script, nil
}

// resolveCustomUserData reads --user-data/--user-data-file's actual content
// against the local filesystem: --user-data-file wins if set, else --user-data
// (either inline text or an "@path" file reference), else "". Pure I/O aside
// from the two package-level flag globals it reads — no AWS — so it's safe to
// call from a dry-run preview (spawn#569) without any of buildUserData's SSH
// key resolution. Extracted from buildUserData so both share one
// implementation instead of the preview re-deriving the same file-read logic.
func resolveCustomUserData() (string, error) {
	if userDataFile != "" {
		// Validate path for security
		if err := security.ValidatePathForReading(userDataFile); err != nil {
			return "", fmt.Errorf("invalid user data file path: %w", err)
		}
		data, err := os.ReadFile(userDataFile)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if userData != "" {
		if strings.HasPrefix(userData, "@") {
			path := userData[1:]
			// Validate path for security
			if err := security.ValidatePathForReading(path); err != nil {
				return "", fmt.Errorf("invalid user data file path: %w", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return string(data), nil
		}
		return userData, nil
	}
	return "", nil
}

// readyGatesForLaunch returns the extra --command readiness gates to declare for
// this launch, beyond the storage gate BuildLinuxBootstrap adds itself.
//
// Only MPI qualifies today. The asymmetry matters: declaring a gate that nothing
// signals makes --command wait the full timeout and then FAIL a launch that
// works today, so a gate is declared only where its producer is certain to be
// appended. That is safe for `spawn array retry`, which is plain-array only —
// MPI arrays are all-or-nothing and never take that path.
func readyGatesForLaunch(mpi bool) []string {
	if mpi {
		return []string{launcher.MPIReadyGate}
	}
	return nil
}

// buildUserData assembles the Linux (or Windows) bootstrap. readyMountPoints are
// the paths --command must see mounted before it starts (#668); pass nil when
// there is no storage script, since there is then nothing to wait for.
func buildUserData(plat *platform.Platform, config *aws.LaunchConfig, storageScript string, readyMountPoints []string) (string, error) {
	if err := userDataOSGuard(config); err != nil {
		return "", err
	}

	// Inject the PUBLIC key of the same keypair we registered with EC2
	// (config.KeyName), so the instance trusts the key `spawn connect` will use.
	publicKey, err := spawnPublicKeyForUserData(plat, config.KeyName)
	if err != nil {
		return "", err
	}
	return buildUserDataForKey(plat, config, publicKey, storageScript, readyMountPoints)
}

// userDataOSGuard refuses workload inputs the target OS's bootstrap would drop.
func userDataOSGuard(config *aws.LaunchConfig) error {
	// The Windows bootstrap runs no workload: buildWindowsUserData takes only the
	// key. The launch guards refuse --command and --user-data for Windows
	// (#1022); this is the backstop at the point where they would be dropped, so
	// a caller that skips the guards gets an error instead of an instance that
	// never runs its workload and never fires --on-complete.
	if config.TargetOS == "windows" {
		if config.JobArrayCommand != "" {
			return fmt.Errorf("--command is not supported on Windows: the Windows bootstrap would not run it (#1022)")
		}
		custom, err := resolveCustomUserData()
		if err != nil {
			return err
		}
		if custom != "" {
			return fmt.Errorf("--user-data is not supported on Windows: the Windows bootstrap would drop it (#1022)")
		}
	}
	return nil
}

// buildUserDataForKey is buildUserData with the instance's trusted public key
// already resolved; an empty key is a keyless (SSM-only) instance. The caller
// has run userDataOSGuard.
func buildUserDataForKey(plat *platform.Platform, config *aws.LaunchConfig, publicKey []byte, storageScript string, readyMountPoints []string) (string, error) {
	// Windows uses a completely different bootstrap: EC2Launch runs a
	// <powershell> block, not bash. It installs SSH and spored and nothing else;
	// the workload inputs were refused above (#1022).
	if config.TargetOS == "windows" {
		return buildWindowsUserData(string(publicKey))
	}

	username := plat.GetUsername()

	// Read custom user data if provided. This is the CLI-only part: resolving
	// --user-data-file and the @path form against the local filesystem.
	customUserData, err := resolveCustomUserData()
	if err != nil {
		return "", err
	}

	// Delegate to the shared, headless bootstrap builder (pkg/launcher) so the
	// CLI and SDK consumers (lagotto, cohort) emit identical spored user-data.
	// --command readiness gates (#668, #664). The MPI gate is declared here, at
	// build time, because the MPI script is appended to this script LATER (by
	// buildJobArrayMemberConfig) and a gate announced after the workload starts
	// waiting would simply be missed. Declaring it on the mpiEnabled flag is safe
	// for `array retry`, which is plain-array only — MPI arrays are
	// all-or-nothing and never take that path.
	readyGates := readyGatesForLaunch(mpiEnabled)

	pluginDecls, err := collectPluginDeclarations()
	if err != nil {
		return "", err
	}

	return launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:         username,
		PublicKey:        publicKey,
		Plugins:          pluginDecls,
		StorageScript:    storageScript,
		ReadyMountPoints: readyMountPoints,
		ReadyGates:       readyGates,
		CustomUserData:   customUserData,
		// Embed --command in user-data instead of the 256-char spawn:command tag
		// (#214/#246). A sweep row's short command still rides the tag; see
		// embeddedCommand.
		Command: embeddedCommand(config),
		// The --command wrapper must write the file spored watches (#1131).
		CompletionFile: config.CompletionFile,
	})
}

// embeddedCommand returns the command to embed in user-data, or "" when it
// rides the spawn:command tag instead. A single launch always embeds, which
// lifts the 256-char tag cap (#214/#246). A sweep row keeps the tag for a short
// one-line command (the per-instance step ref the tag path was built for), and
// embeds one the tag cannot carry: before #1242 such a row reached
// RunInstances with an oversized tag and was refused.
func embeddedCommand(config *aws.LaunchConfig) string {
	if config.SweepID != "" && commandFitsTag(config.JobArrayCommand) {
		return ""
	}
	return config.JobArrayCommand
}

// commandFitsTag reports whether command can ride the spawn:command tag: EC2
// caps a tag value at 256 characters, and the bootstrap reads it back as a
// single line.
func commandFitsTag(command string) bool {
	return len(command) <= 256 && !strings.ContainsAny(command, "\r\n")
}

// collectPluginDeclarations merges plugin refs from --plugin flags and --config file.
//
// A --config file that cannot be read or parsed is an error, never an empty
// plugin list (#1116): the error used to be dropped here, so a typo'd path
// launched an instance with none of the plugins it named.
func collectPluginDeclarations() ([]plugin.Declaration, error) {
	var decls []plugin.Declaration

	// From --config YAML file.
	if launchConfigFile != "" {
		cfg, err := loadLaunchConfig(launchConfigFile)
		if err != nil {
			return nil, fmt.Errorf("--config: %w", err)
		}
		decls = append(decls, cfg.Plugins...)
	}

	// From --plugin flags (simple refs without per-plugin config).
	for _, ref := range launchPlugins {
		decls = append(decls, plugin.Declaration{Ref: ref})
	}

	return decls, nil
}
