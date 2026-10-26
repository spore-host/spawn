package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// The spored bootstrap for a detached sweep's members (#1323).
//
// The sweep-orchestrator Lambda launches a detached sweep's members, and it used
// to launch them with no UserData, so spored never ran on one: no in-instance
// TTL, idle stop, cost limit or completion handling. Only the TTL reaper ended
// them, on its next cycle, in the regions it covers.
//
// The CLI renders each member's user data here, with buildUserData, the builder
// the foreground sweep path uses for the same member config, and ships it in the
// uploaded params under detachedUserDataKey; the Lambda passes it to
// RunInstances verbatim. Rendering here rather than in the Lambda because the
// inputs are the CLI's: the local public key, --plugin/--config plugins,
// --user-data contents, and the spawn release whose published bootstrap body the
// stub fetches (launcher.CallerVersion; a dev build inlines the body, as it does
// on every other path). The Lambda is built by a plain `go build` and knows none
// of them.
//
// Size: a member's user data is what a foreground member's is, inside EC2's
// 16 KiB. The params object is S3, with no practical limit, and when every
// member's user data is the same, which is the usual case, it is written once
// under defaults: rather than per row.

// detachedUserDataKey is the param key the orchestrator reads a member's user
// data from. It must equal userDataParam in lambda/sweep-orchestrator/spored.go;
// TestDetachedSweepSeedsEveryKeyTheLambdaReads holds the two together.
const detachedUserDataKey = "spawn_user_data"

// scheduledSweepIDPlaceholder stands in for the sweep ID of a schedule's runs,
// which scheduler-handler mints when the schedule fires. The bootstrap only asks
// whether the launch is a sweep's (nonSweepCommand: a sweep's command rides its
// tag), never what the ID is.
const scheduledSweepIDPlaceholder = "scheduled"

// sporedProfileEnsurer creates spored's instance profile if it is missing.
// *aws.Client satisfies it.
type sporedProfileEnsurer interface {
	SetupSporedIAMRole(ctx context.Context) (string, error)
}

// detachedBootstrap is what rendering a detached sweep's bootstrap needs beyond
// the param file.
type detachedBootstrap struct {
	plat *platform.Platform
	// osOf returns a member's target OS ("linux" or "windows") from its region
	// and AMI, as the foreground path's resolveTargetOS does.
	osOf func(region, ami string) string
}

// newDetachedBootstrap returns the production detachedBootstrap: the local
// platform, and the target OS resolved per (region, AMI) with client, memoized.
func newDetachedBootstrap(ctx context.Context, client *aws.Client, plat *platform.Platform) detachedBootstrap {
	cache := map[string]string{}
	return detachedBootstrap{
		plat: plat,
		osOf: func(region, ami string) string {
			k := region + "|" + ami
			if target, ok := cache[k]; ok {
				return target
			}
			target := resolveTargetOS(ctx, client, region, ami, osFlag)
			cache[k] = target
			return target
		},
	}
}

// seedDetachedBootstrap renders every member's spored user data into pf, after
// seedDetachedLaunchKeys has settled each member's AMI, key and region. base is
// the CLI's launch config (empty for a schedule), sweepID and sweepName the
// sweep's.
//
// Any detachedUserDataKey the file itself carried is dropped first: the key is
// spawn's, and a value the user wrote would otherwise be booted in place of
// spored (reservedRowKeys refuses it up front; this is the backstop).
func seedDetachedBootstrap(pf *ParamFileFormat, base aws.LaunchConfig, sweepID, sweepName string, b detachedBootstrap) error {
	if pf.Defaults == nil {
		pf.Defaults = map[string]interface{}{}
	}
	delete(pf.Defaults, detachedUserDataKey)
	for _, row := range pf.Params {
		delete(row, detachedUserDataKey)
	}
	if len(pf.Params) == 0 {
		return nil
	}

	userData := make([]string, len(pf.Params))
	for i, row := range pf.Params {
		cfg, err := buildLaunchConfigFromParams(base, pf.Defaults, row, sweepID, sweepName, i, len(pf.Params))
		if err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
		cfg.TargetOS = b.osOf(cfg.Region, cfg.AMI)
		if err := windowsLifecycleGuard(&cfg); err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
		decls, err := collectPluginDeclarations()
		if err != nil {
			return err
		}
		if err := windowsPluginGuard(cfg.TargetOS, decls); err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
		ud, err := detachedMemberUserData(b.plat, &cfg)
		if err != nil {
			return fmt.Errorf("param set %d: build spored user data: %w", i, err)
		}
		userData[i] = ud
	}

	same := true
	for _, ud := range userData[1:] {
		if ud != userData[0] {
			same = false
			break
		}
	}
	if same {
		pf.Defaults[detachedUserDataKey] = userData[0]
		return nil
	}
	for i := range pf.Params {
		if pf.Params[i] == nil {
			pf.Params[i] = map[string]interface{}{}
		}
		pf.Params[i][detachedUserDataKey] = userData[i]
	}
	return nil
}

// detachedMemberUserData is a member's encoded user data: what the foreground
// path sets as cfg.UserData for the same config. The one difference is a member
// with no key pair whose launching machine has no public key either: the
// foreground path would have made a key pair first, the detached one has no
// key to install, so the member is keyless and `spawn connect` injects a key
// over SSM, as it does for any keyless instance.
func detachedMemberUserData(plat *platform.Platform, cfg *aws.LaunchConfig) (string, error) {
	if err := userDataOSGuard(cfg); err != nil {
		return "", err
	}
	var publicKey []byte
	_, noLocalKey := plat.ReadPublicKey()
	if cfg.KeyName != "" || noLocalKey == nil {
		pub, err := spawnPublicKeyForUserData(plat, cfg.KeyName)
		if err != nil {
			return "", err
		}
		publicKey = pub
	}
	script, err := buildUserDataForKey(plat, cfg, publicKey, "", nil)
	if err != nil {
		return "", err
	}
	return encodeUserDataForOS(script, cfg.TargetOS), nil
}

// ensureDetachedInstanceProfile makes sure every member has an instance
// profile: spored needs its credentials to read its tags and write its
// heartbeat. A member with no iam_role of its own, and no sweep-wide one, gets
// spored-instance-profile, created here if missing, the profile the foreground
// path ensures for the same case. spore.host creates it; the user never has to.
func ensureDetachedInstanceProfile(ctx context.Context, e sporedProfileEnsurer, pf *ParamFileFormat) error {
	if stringParam(pf.Defaults, "iam_role") != "" {
		return nil
	}
	need := false
	for _, row := range pf.Params {
		if stringParam(row, "iam_role") == "" {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	profile, err := e.SetupSporedIAMRole(ctx)
	if err != nil {
		return fmt.Errorf("set up spored's instance profile: %w", err)
	}
	if pf.Defaults == nil {
		pf.Defaults = map[string]interface{}{}
	}
	pf.Defaults["iam_role"] = profile
	fmt.Fprintf(os.Stderr, "✓ IAM: members without an iam_role use %s\n", profile)
	return nil
}
