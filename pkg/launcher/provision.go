package launcher

import (
	"context"
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/plugin"
)

// DefaultUsername is the local user created on instances provisioned headlessly
// (no $USER to read). It matches the Amazon Linux default that `spawn connect`
// assumes, so connect works without --user.
const DefaultUsername = "ec2-user"

// Options tune a headless Provision. The zero value is valid: Linux,
// ec2-user, SSM-only (no SSH key), no plugins.
type Options struct {
	// Username overrides the local user the bootstrap creates. Empty =>
	// DefaultUsername.
	Username string
	// PublicKey is the SSH public key (authorized_keys line) to trust. Empty =>
	// SSM-only instance with no SSH key; `spawn connect` can inject one later
	// over SSM (lagotto#19 — the capacity-poller Lambda has no key on disk).
	PublicKey []byte
	// Plugins are spored plugin declarations to bake into the instance.
	Plugins []plugin.Declaration
	// CustomUserData is appended verbatim after the bootstrap (already-resolved
	// text; headless callers have no @file indirection).
	CustomUserData string
}

// Provision performs the headless equivalent of `spawn launch`: it fills in the
// pieces the CLI normally resolves before calling aws.Client.Launch, so SDK
// consumers get a fully-functional spore (spored installed, command executed,
// on-complete/pre-stop/idle/TTL enforced) instead of a naked instance.
//
// Steps, each skipped when the caller already supplied the value:
//  1. AMI: if config.AMI is empty, auto-detect the recommended AL2023 AMI for
//     the region + instance type (arch and GPU are derived from the type).
//  2. IAM: if config.IamInstanceProfile is empty, set up the shared spored
//     instance profile (also attaches AmazonSSMManagedInstanceCore, so an
//     SSM-only / keyless instance is still reachable).
//  3. User-data: if config.UserData is empty, build the spored Linux bootstrap.
//     A caller that pre-set UserData (e.g. a fully custom script) is left alone.
//  4. Launch.
//
// Windows is out of scope here (no spored-on-Windows headless path yet, #77);
// callers must build Windows user-data themselves.
func Provision(ctx context.Context, client *aws.Client, config aws.LaunchConfig, opts Options) (*aws.LaunchResult, error) {
	if config.InstanceType == "" {
		return nil, fmt.Errorf("provision: instance type is required")
	}
	if config.Region == "" {
		return nil, fmt.Errorf("provision: region is required")
	}
	if config.TargetOS == "windows" {
		return nil, fmt.Errorf("provision: Windows is not supported by the headless launcher (build Windows user-data via the CLI)")
	}

	// 1. AMI auto-detection (lagotto#19 issue #1). GetRecommendedAMI derives the
	// CPU architecture and GPU-ness from the instance type, so a g5/arm64 type
	// gets the right AMI without the caller specifying anything.
	if config.AMI == "" {
		ami, err := client.GetRecommendedAMI(ctx, config.Region, config.InstanceType)
		if err != nil {
			return nil, fmt.Errorf("provision: auto-detect AMI for %s in %s: %w", config.InstanceType, config.Region, err)
		}
		config.AMI = ami
	}

	// 2. spored IAM instance profile. Without it spored can't read its tags or
	// self-terminate, and SSM connect can't fall back. The CLI's default path
	// uses exactly this profile.
	if config.IamInstanceProfile == "" {
		profile, err := client.SetupSporedIAMRole(ctx)
		if err != nil {
			return nil, fmt.Errorf("provision: set up spored IAM role: %w", err)
		}
		config.IamInstanceProfile = profile
	}

	// 3. spored bootstrap user-data (lagotto#19 issues #2/#3 root cause). This is
	// the script that installs spored and makes the spawn:command / on-complete /
	// pre-stop / idle tags actually do something.
	if config.UserData == "" {
		username := opts.Username
		if username == "" {
			username = DefaultUsername
		}
		bootstrap, err := BuildLinuxBootstrap(BootstrapConfig{
			Username:       username,
			PublicKey:      opts.PublicKey,
			Plugins:        opts.Plugins,
			CustomUserData: opts.CustomUserData,
		})
		if err != nil {
			return nil, fmt.Errorf("provision: build bootstrap: %w", err)
		}
		// RunInstances requires base64 user-data (cloud-init also gunzips it).
		// Encode here — assigning the raw script makes RunInstances fail with
		// "Invalid BASE64 encoding of user data" (#127).
		config.UserData = EncodeLinuxUserData(bootstrap)
		// Tag the primary user so spored runs the pre-stop hook as them, not root (#63).
		if config.Username == "" {
			config.Username = username
		}
	}

	// 4. Launch.
	result, err := client.Launch(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("provision: launch: %w", err)
	}
	return result, nil
}
