package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/scttfrdmn/strata/pkg/strata"
	"github.com/scttfrdmn/strata/spec"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/sshkey"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"gopkg.in/yaml.v3"
)

// resolveStrataEnvironment resolves a Strata formation or profile to a lockfile
// S3 URI, which is set as the strata:lockfile-s3-uri EC2 instance tag at launch.
// strata-agent on the instance reads this tag at boot and mounts the environment.
func resolveStrataEnvironment(ctx context.Context, formation, profilePath, registry string) (string, error) {
	var profile *spec.Profile
	if profilePath != "" {
		data, err := os.ReadFile(profilePath)
		if err != nil {
			return "", fmt.Errorf("read profile: %w", err)
		}
		if err := yaml.Unmarshal(data, &profile); err != nil {
			return "", fmt.Errorf("parse profile: %w", err)
		}
	} else {
		profile = &spec.Profile{
			Name:     formation,
			Base:     spec.BaseRef{OS: "al2023"},
			Software: []spec.SoftwareRef{{Formation: formation}},
		}
	}
	c, err := strata.NewClient(ctx, strata.Options{RegistryURL: registry})
	if err != nil {
		return "", fmt.Errorf("new client: %w", err)
	}
	lf, err := c.Resolve(ctx, profile, strata.ResolveOptions{})
	if err != nil {
		return "", fmt.Errorf("resolve: %w", err)
	}
	uri, err := c.UploadLockfile(ctx, lf)
	if err != nil {
		return "", fmt.Errorf("upload lockfile: %w", err)
	}
	return uri, nil
}

// resolveTargetOS decides the instance OS: an explicit --os flag wins (for
// custom AMIs whose platform metadata is unset), otherwise auto-detect from the
// AMI via IsWindowsAMI. Returns "windows" or "linux".
func resolveTargetOS(ctx context.Context, awsClient *aws.Client, region, amiID, osFlag string) string {
	switch strings.ToLower(strings.TrimSpace(osFlag)) {
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	}
	if awsClient.IsWindowsAMI(ctx, region, amiID) {
		return "windows"
	}
	return "linux"
}

// defaultWindowsInstanceType is the default for `--os windows` when the user
// doesn't pass --instance-type. Windows must not default to a burstable type:
// the Sysprep/OOBE first boot starves on CPU credits and takes ~20+ min (#95).
const defaultWindowsInstanceType = "m7i.xlarge"

// isBurstableInstanceType reports whether an instance type is a burstable
// (t-family) type, e.g. "t3.large", "t4g.micro". Burstable CPU credits make
// Windows first-boot painfully slow, so we reject them for Windows.
func isBurstableInstanceType(instanceType string) bool {
	return strings.HasPrefix(instanceType, "t") && strings.Contains(instanceType, ".") &&
		(strings.HasPrefix(instanceType, "t2.") || strings.HasPrefix(instanceType, "t3.") ||
			strings.HasPrefix(instanceType, "t3a.") || strings.HasPrefix(instanceType, "t4g."))
}

// guardWindowsInstanceType rejects burstable instance types for Windows, with a
// clear, actionable error. Returns nil for non-Windows or acceptable types.
func guardWindowsInstanceType(targetOS, instanceType string) error {
	if targetOS != "windows" {
		return nil
	}
	if isBurstableInstanceType(instanceType) {
		return fmt.Errorf("instance type %q is burstable (t-family); Windows first boot starves on burst CPU credits and takes ~20+ min — choose a non-burstable type (default for Windows is %s)", instanceType, defaultWindowsInstanceType)
	}
	return nil
}

// preflightInstanceConstraints validates that the requested instance type
// supports the requested features (MPI cluster placement group, EFA,
// hibernation) BEFORE any AWS resources are created, with actionable errors
// (#110). One DescribeInstanceTypes call backs all checks. HPC types are exempt
// from the MPI/placement-group requirement — they use AWS HPC networking and
// spawn skips the placement group for them (#104), so --mpi alone is fine.
func preflightInstanceConstraints(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, wantMPI, wantEFA, wantHibernate bool) error {
	if !wantMPI && !wantEFA && !wantHibernate {
		return nil // nothing feature-specific to check
	}
	// truffle is the instance-type capability authority — consume it rather than
	// re-querying EC2 from spawn. Build a truffle client from spawn's AWS config
	// so creds/region match.
	tc := truffleaws.NewClientFromConfig(awsClient.Config())
	caps, err := tc.GetCapabilities(ctx, config.InstanceType, config.Region)
	if err != nil {
		return fmt.Errorf("pre-flight instance-type check: %w", err)
	}
	if !caps.Found {
		return fmt.Errorf("instance type %q not found in region %s", config.InstanceType, config.Region)
	}

	// --efa: must support EFA.
	if wantEFA && !caps.EFA {
		return fmt.Errorf("instance type %q does not support EFA (required for --efa).\n       Find EFA-capable types: truffle find \"%s\" efa  (e.g. c5n.18xlarge, hpc6a.48xlarge)",
			config.InstanceType, instanceFamilyHint(config.InstanceType))
	}

	// --hibernate / --hibernate-on-idle: must support hibernation.
	if wantHibernate && !caps.Hibernation {
		return fmt.Errorf("instance type %q does not support hibernation (required for --hibernate/--hibernate-on-idle).\n       Choose a hibernation-capable type, or drop the hibernation flag.", config.InstanceType)
	}

	// --mpi: needs a cluster placement group UNLESS it's an HPC type (which spawn
	// skips the placement group for). So only block --mpi when neither holds.
	if wantMPI && !caps.ClusterPlacement && !isHPCInstanceType(config.InstanceType) {
		return fmt.Errorf("instance type %q does not support cluster placement groups (needed for --mpi).\n       Use an MPI-capable type (e.g. c5n.18xlarge, c6i.32xlarge) or an HPC type (hpc6a/hpc7a/hpc7g), or run: truffle find \"%s\" efa",
			config.InstanceType, instanceFamilyHint(config.InstanceType))
	}
	return nil
}

// instanceFamilyHint returns a glob hint for the instance's family for use in
// suggested truffle commands, e.g. "c5n.18xlarge" -> "c5n*".
func instanceFamilyHint(instanceType string) string {
	if i := strings.IndexByte(instanceType, '.'); i > 0 {
		return instanceType[:i] + "*"
	}
	return instanceType
}

// isHPCInstanceType reports whether the type is in the AWS HPC family, which
// gets low-latency networking from HPC infrastructure rather than placement
// groups (so --mpi is valid without a cluster placement group). Detected by the
// "hpc" family prefix rather than a hardcoded list, so new HPC families
// (hpc6a/hpc6id/hpc7a/hpc7g/hpc8a/… as of June 2026, and future ones) are
// covered automatically — the EC2 naming convention is the contract. A real
// family is "hpc" followed by a generation digit (hpc6a, hpc7g…), so we require
// the digit to avoid matching a stray "hpc.weird".
func isHPCInstanceType(instanceType string) bool {
	const p = "hpc"
	if !strings.HasPrefix(instanceType, p) || len(instanceType) <= len(p) {
		return false
	}
	c := instanceType[len(p)]
	return c >= '0' && c <= '9'
}

// windowsLifecycleGuard enforces cost safety for Windows launches. Windows has
// no in-instance spored yet (#77), so idle-timeout cannot work and the only
// thing that will stop the instance is its TTL plus the server-side reaper
// backstop (#70). We therefore REQUIRE --ttl for Windows and warn loudly that no
// agent runs. Linux is unaffected.
func windowsLifecycleGuard(config *aws.LaunchConfig) error {
	if config.TargetOS != "windows" {
		return nil
	}
	// spored now runs on Windows as a Service (#77), so idle-timeout, completion,
	// and pre-stop work in-instance — same as Linux. We still require a timeout
	// (TTL or idle) so a Windows box can't run unbounded if the agent fails to
	// install; the server-side reaper (#70) backstops the TTL deadline regardless.
	if config.TTL == "" && config.IdleTimeout == "" {
		return fmt.Errorf("Windows instances require a timeout: set --ttl (hard deadline) " +
			"and/or --idle-timeout. The in-instance agent enforces these and the server-side " +
			"reaper backstops the TTL deadline.\n  Re-run with e.g. --ttl 8h")
	}
	return nil
}

func setupSSHKey(ctx context.Context, awsClient *aws.Client, region, amiID string, plat *platform.Platform) (string, error) {
	// Choose the keypair algorithm from the target OS. Windows requires RSA —
	// the EC2 Administrator password (GetPasswordData) can only be decrypted with
	// an RSA private key; ED25519 cannot. Everything else defaults to ED25519.
	algo := sshkey.ED25519
	if awsClient.IsWindowsAMI(ctx, region, amiID) {
		algo = sshkey.RSA
	}

	// Find-or-create spawn's managed keypair under ~/.spawn/keys (separate from
	// the user's personal ~/.ssh). Generated in-process — no ssh-keygen shell-out.
	kp, err := sshkey.EnsureKey(plat.HomeDir, plat.GetUsername(), algo)
	if err != nil {
		return "", fmt.Errorf("failed to ensure spawn SSH key: %w", err)
	}

	// Reuse an already-imported EC2 key with the same fingerprint, so re-launches
	// don't re-import.
	fingerprint, err := sshkey.Fingerprint(kp.PublicKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to get key fingerprint: %w", err)
	}
	existingKeyName, err := awsClient.FindKeyPairByFingerprint(ctx, region, fingerprint)
	if err != nil {
		return "", fmt.Errorf("failed to search for existing key: %w", err)
	}
	if existingKeyName != "" {
		return existingKeyName, nil
	}

	// Import under the algorithm-qualified name (spawn-key-<user> /
	// spawn-key-<user>-rsa) so both can coexist in EC2.
	publicKey, err := os.ReadFile(kp.PublicKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}
	if err := awsClient.ImportKeyPair(ctx, region, kp.Name, publicKey); err != nil {
		return "", fmt.Errorf("failed to import key pair: %w", err)
	}
	return kp.Name, nil
}
