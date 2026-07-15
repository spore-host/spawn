package cmd

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/input"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/sshkey"
	"github.com/spore-host/spawn/pkg/storage"
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
	if len(specs) == 0 {
		return nil
	}
	vols := make([]userdata.AttachedVolume, 0, len(specs))
	for i, s := range specs {
		vols = append(vols, userdata.AttachedVolume{
			DeviceName: aws.AttachDeviceName(i),
			MountPoint: s.MountPoint,
			ReadOnly:   s.ReadOnly,
		})
	}
	return vols
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
	}
	// --name implies DNS registration; --dns overrides the DNS portion only.
	if dnsName == "" && name != "" {
		dnsName = name
	} else if name == "" && dnsName != "" {
		name = dnsName
	}
	if dnsName != "" {
		config.DNSName = dnsName
	}
	if slackWorkspaceID != "" {
		config.SlackWorkspaceID = slackWorkspaceID
		// The spore-bot Lambda Function URL — hard-coded for hosted spore.host;
		// can be overridden via SPORE_BOT_NOTIFY_URL env var for self-hosted deployments.
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
	if onComplete != "" {
		config.OnComplete = onComplete
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
				return nil, fmt.Errorf("--fsx-ttl is only valid with --fsx-lifecycle=durable (ephemeral FSx is reaped when the instance terminates)")
			}
		case "durable":
			if fsxTTL == "" {
				return nil, fmt.Errorf("--fsx-lifecycle=durable requires --fsx-ttl (e.g. 7d) — a durable FSx must have a death clock so it can't bill indefinitely")
			}
			if _, err := parseDuration(fsxTTL); err != nil {
				return nil, fmt.Errorf("invalid --fsx-ttl %q: %w", fsxTTL, err)
			}
		case "":
			return nil, fmt.Errorf("--fsx-create requires --fsx-lifecycle: 'ephemeral' (reaped with this instance) or 'durable' (persists; needs --fsx-ttl). An FSx costs money and holds your results — choose its lifetime explicitly")
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

	return config, nil
}

// encodeUserData gzip-compresses the script and base64-encodes the result.
// cloud-init on Amazon Linux 2023 and Ubuntu supports gzip+base64 user-data,
// which keeps the payload well under EC2's 16 KB limit even when combining
// spored bootstrap + MPI + FSx mount scripts (fixes #304).
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

  $dir = Join-Path $env:ProgramFiles 'spored'
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $exe = Join-Path $dir 'spored.exe'
  $bucket = "spawn-binaries-$region"
  $ok = $false
  foreach ($uri in @("s3://$bucket/spawn/spored-windows-amd64.exe","s3://$bucket/spored-windows-amd64.exe","s3://spawn-binaries-us-east-1/spawn/spored-windows-amd64.exe")) {
    & $aws s3 cp $uri $exe --region $region 2>$null
    if ($LASTEXITCODE -eq 0 -and (Test-Path $exe)) { $ok = $true; break }
  }
  if ($ok) {
    & $exe service install $exe
    & $exe service start
  } else {
    Write-Output "spawn: could not download spored.exe from S3"
  }
} catch {
  Write-Output "spawn spored install warning: $_"
}
</powershell>
<persist>false</persist>`, key)

	return script, nil
}

func buildUserData(plat *platform.Platform, config *aws.LaunchConfig, storageScript string) (string, error) {
	// Inject the PUBLIC key of the same keypair we registered with EC2
	// (config.KeyName), so the instance trusts the key `spawn connect` will use.
	publicKey, err := spawnPublicKeyForUserData(plat, config.KeyName)
	if err != nil {
		return "", err
	}

	// Windows uses a completely different bootstrap: EC2Launch runs a
	// <powershell> block, not bash, and there is no spored yet (#77). Branch
	// early to a minimal PowerShell script.
	if config.TargetOS == "windows" {
		return buildWindowsUserData(string(publicKey))
	}

	username := plat.GetUsername()

	// Read custom user data if provided. This is the CLI-only part: resolving
	// --user-data-file and the @path form against the local filesystem. The
	// resolved text is handed to the shared launcher below.
	customUserData := ""

	if userDataFile != "" {
		// Validate path for security
		if err := security.ValidatePathForReading(userDataFile); err != nil {
			return "", fmt.Errorf("invalid user data file path: %w", err)
		}
		data, err := os.ReadFile(userDataFile)
		if err != nil {
			return "", err
		}
		customUserData = string(data)
	} else if userData != "" {
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
			customUserData = string(data)
		} else {
			customUserData = userData
		}
	}

	// Delegate to the shared, headless bootstrap builder (pkg/launcher) so the
	// CLI and SDK consumers (lagotto, cohort) emit identical spored user-data.
	return launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:       username,
		PublicKey:      publicKey,
		Plugins:        collectPluginDeclarations(),
		StorageScript:  storageScript,
		CustomUserData: customUserData,
		// Embed --command in user-data instead of the 256-char spawn:command tag
		// (#214/#246). Sweeps deliver per-instance commands via the tag (a short
		// step ref), so only embed for a non-sweep launch — SweepID set means the
		// tag path owns the command.
		Command: nonSweepCommand(config),
	})
}

// nonSweepCommand returns the --command to embed in user-data, or "" when the
// command should ride the spawn:command tag instead (the parameter-sweep path,
// which sets a short per-instance command tag). Embedding lifts the 256-char tag
// cap for the common single-launch case (#214/#246).
func nonSweepCommand(config *aws.LaunchConfig) string {
	if config.SweepID != "" {
		return ""
	}
	return config.JobArrayCommand
}

// collectPluginDeclarations merges plugin refs from --plugin flags and --config file.
func collectPluginDeclarations() []plugin.Declaration {
	var decls []plugin.Declaration

	// From --config YAML file.
	if launchConfigFile != "" {
		if cfg, err := loadLaunchConfig(launchConfigFile); err == nil {
			decls = append(decls, cfg.Plugins...)
		}
	}

	// From --plugin flags (simple refs without per-plugin config).
	for _, ref := range launchPlugins {
		decls = append(decls, plugin.Declaration{Ref: ref})
	}

	return decls
}
