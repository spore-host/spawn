package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/spotrequest"
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
// on every other path). The Lambda has none of the first three. It renders a
// bootstrap itself only for params from a CLI that rendered none
// (lambda/sweep-orchestrator/legacy.go, #1368).
//
// Size: a member's user data is what a foreground member's is, inside EC2's
// 16 KiB. The params object is S3, with no practical limit, and when every
// member's user data is the same, which is the usual case, it is written once
// under defaults: rather than per row.

// detachedUserDataKey is the param key the orchestrator reads a member's user
// data from. It must equal userDataParam in lambda/sweep-orchestrator/spored.go;
// TestDetachedSweepSeedsEveryKeyTheLambdaReads holds the two together.
const detachedUserDataKey = "spawn_user_data"

// detachedTagsKey is the param key carrying a member's instance tags, rendered
// by the tag builder a foreground launch uses (#1330). It must equal tagsParam
// in lambda/sweep-orchestrator/spored.go; TestDetachedSweepSeedsEveryKeyTheLambdaReads
// holds the two together. Always per row: every member's differs, if only in
// spawn:sweep-index.
const detachedTagsKey = "spawn_tags"

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
	// tagsOf renders a member's instance tags as a foreground launch of cfg
	// would write them: aws.LaunchTagger in production.
	tagsOf func(cfg aws.LaunchConfig) (map[string]string, error)
	// settingsOf resolves a member's RunInstances-side settings as a foreground
	// launch of cfg would (#1353): aws.Client.ResolveLaunchSettings in production.
	settingsOf func(cfg aws.LaunchConfig) aws.LaunchSettings
	// storageOf renders a member's storage-mount script and the mount points
	// its --command waits for, as a foreground member's (#1366), filling in the
	// FSx mount name its tags carry: sweepStorage.member in production.
	storageOf func(cfg *aws.LaunchConfig) (string, []string, error)
	// efaOf resolves an --efa member's EFA interface count as a foreground
	// member's (#1407), so detachedSettingsGuard refuses the count it would
	// launch with: resolveSweepMemberEFA through memoEFACapacity in production.
	efaOf func(cfg *aws.LaunchConfig) error
	// privateNetOf is a --no-public-ip member's subnet check (#1437), run at
	// --detach with the caller's credentials because the orchestrator cannot
	// make it: with only an AZ it picks and pins the subnet, then refuses one
	// that cannot reach what spored and the SSM agent need, unless
	// --allow-missing-endpoints. aws.Client.PreflightNoPublicIP in production.
	privateNetOf func(cfg *aws.LaunchConfig) error
}

// newDetachedBootstrap returns the production detachedBootstrap: the local
// platform, and the target OS resolved per (region, AMI) with client, memoized.
func newDetachedBootstrap(ctx context.Context, client *aws.Client, plat *platform.Platform) detachedBootstrap {
	cache := map[string]string{}
	var tagger *aws.LaunchTagger
	storage := newSweepStorage(client, os.Stderr)
	efa := memoEFACapacity(ctx, client)
	return detachedBootstrap{
		privateNetOf: func(cfg *aws.LaunchConfig) error {
			_, err := preflightNoPublicIPFor(ctx, client, cfg, nil, os.Stderr)
			return err
		},
		efaOf: func(cfg *aws.LaunchConfig) error {
			return resolveSweepMemberEFA(cfg, efa)
		},
		storageOf: func(cfg *aws.LaunchConfig) (string, []string, error) {
			return storage.member(ctx, cfg)
		},
		tagsOf: func(cfg aws.LaunchConfig) (map[string]string, error) {
			if tagger == nil {
				t, err := client.NewLaunchTagger(ctx)
				if err != nil {
					return nil, err
				}
				tagger = t
			}
			return tagger.TagValues(ctx, cfg)
		},
		settingsOf: func(cfg aws.LaunchConfig) aws.LaunchSettings {
			return client.ResolveLaunchSettings(ctx, cfg)
		},
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
	for _, k := range []string{detachedUserDataKey, detachedTagsKey, detachedLaunchSettingsKey} {
		delete(pf.Defaults, k)
		for _, row := range pf.Params {
			delete(row, k)
		}
	}
	if len(pf.Params) == 0 {
		return nil
	}

	userData := make([]string, len(pf.Params))
	memberTags := make([]map[string]interface{}, len(pf.Params))
	memberSettings := make([]aws.LaunchSettings, len(pf.Params))
	var defaultedVerbs []string
	for i, row := range pf.Params {
		cfg, err := buildLaunchConfigFromParams(base, pf.Defaults, row, sweepID, sweepName, i, len(pf.Params))
		if err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
		// The foreground loop's per-member defaults, the 1h idle timeout for a
		// member with no ttl and no idle_timeout among them (#1330).
		if defaultSweepMember(&cfg, base, sweepName, i) {
			if v := idleActionVerb(&cfg); !slices.Contains(defaultedVerbs, v) {
				defaultedVerbs = append(defaultedVerbs, v)
			}
		}
		if !orchestratorPassableProfile(cfg.IamInstanceProfile) {
			return unpassableProfileError(fmt.Sprintf("param set %d", i), cfg.IamInstanceProfile)
		}
		// --efa's interface count, resolved as a foreground member's is
		// (#1407), so the refusal below sees the count the member would
		// launch with: --efa on a multi-card type is refused, never launched
		// with one interface.
		if cfg.EFAEnabled {
			if b.efaOf == nil {
				return fmt.Errorf("param set %d: no EFA resolver: --efa would launch one interface whatever the type", i)
			}
			if err := b.efaOf(&cfg); err != nil {
				return fmt.Errorf("param set %d: %w", i, err)
			}
		}
		// What the orchestrator cannot launch as configured is refused here,
		// before anything is uploaded (#1353).
		if err := detachedSettingsGuard(cfg); err != nil {
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
		// --no-public-ip: the member's subnet, pinned and checked (#1437).
		// After the target OS (a Windows bootstrap needs NAT for its AWS CLI
		// installer) and before the settings, which carry the pinned subnet:
		// an AZ-only row would otherwise let EC2 pick one nobody checked.
		if cfg.NoPublicIP {
			if b.privateNetOf == nil {
				return fmt.Errorf("param set %d: no subnet check: --no-public-ip would launch in an unchecked subnet", i)
			}
			if err := b.privateNetOf(&cfg); err != nil {
				return fmt.Errorf("param set %d: %w", i, err)
			}
		}
		// Before the tags: the FSx mount name the lookup finds is one of them.
		if b.storageOf == nil {
			return fmt.Errorf("param set %d: no storage renderer: its mounts would be dropped", i)
		}
		storageScript, ready, err := b.storageOf(&cfg)
		if err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
		ud, err := detachedMemberUserData(b.plat, &cfg, storageScript, ready)
		if err != nil {
			return fmt.Errorf("param set %d: build spored user data: %w", i, err)
		}
		userData[i] = ud

		tags, err := b.tagsOf(cfg)
		if err != nil {
			return fmt.Errorf("param set %d: build instance tags: %w", i, err)
		}
		if err := detachedSpotGuard(tags); err != nil {
			return fmt.Errorf("param set %d: %w", i, err)
		}
		memberTags[i] = encodeDetachedTags(tags)
		memberSettings[i] = b.settingsOf(cfg)
	}
	if len(defaultedVerbs) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h for sweep members with no ttl or idle_timeout\n")
		fmt.Fprintf(os.Stderr, "   They will %s after 1 hour of inactivity.\n", strings.Join(defaultedVerbs, " or "))
		fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n\n")
	}

	for i := range pf.Params {
		if pf.Params[i] == nil {
			pf.Params[i] = map[string]interface{}{}
		}
		pf.Params[i][detachedTagsKey] = memberTags[i]
		pf.Params[i][detachedLaunchSettingsKey] = memberSettings[i]
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
		pf.Params[i][detachedUserDataKey] = userData[i]
	}
	return nil
}

// encodeDetachedTags is a member's tags as the orchestrator reads them: one
// JSON object, key to value.
func encodeDetachedTags(tags map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}

// detachedSpotGuard refuses a Spot member whose lifecycle needs a persistent
// Spot request. A foreground launch makes one when the instance may be stopped
// (an idle timeout that stops, on_complete stop or hibernate, #922), because EC2
// cannot stop an instance from a one-time request. The orchestrator makes
// one-time requests only: it has no way to cancel a persistent one before
// terminating its instance, and a persistent request whose instance is
// terminated launches a replacement. So such a member would run with a stop
// that EC2 refuses.
func detachedSpotGuard(tags map[string]string) error {
	if tags[spotrequest.TypeTag] != spotrequest.TypePersistent {
		return nil
	}
	return fmt.Errorf("this Spot member would need a persistent Spot request, because its lifecycle stops " +
		"the instance (an idle timeout, including the 1h default for a member with no ttl, or on_complete " +
		"stop/hibernate), and a detached sweep launches one-time Spot requests, which EC2 cannot stop. " +
		"Give it a ttl (ttl: or --ttl) and no idle_timeout, and on_complete: terminate if it has an " +
		"on_complete, or run the sweep with --no-detach")
}

// detachedMemberUserData is a member's encoded user data: what the foreground
// path sets as cfg.UserData for the same config. The one difference is a member
// with no key pair whose launching machine has no public key either: the
// foreground path would have made a key pair first, the detached one has no
// key to install, so the member is keyless and `spawn connect` injects a key
// over SSM, as it does for any keyless instance.
func detachedMemberUserData(plat *platform.Platform, cfg *aws.LaunchConfig, storageScript string, readyMountPoints []string) (string, error) {
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
	// The launch config object (#1359). A foreground launch stores it in the
	// staging bucket for spored to fetch; this member is launched later by the
	// sweep-orchestrator Lambda, which cannot write to this account's staging
	// bucket, and a schedule replays this same user data for as long as it
	// runs, past any expiry. So the object rides in the user data and is
	// seeded into spored's cache of it, which spored checks against the
	// member's spawn:config tag like any other.
	object, _, err := aws.LaunchConfigObject(*cfg)
	if err != nil {
		return "", err
	}
	if object != nil && cfg.TargetOS == "windows" {
		return "", fmt.Errorf("a detached sweep cannot launch a Windows member with spored settings fixed at launch " +
			"(notifications, webhooks, --active-ports/--active-processes, a web app, --idle-cpu-threshold, metrics or " +
			"tracing details): the Windows bootstrap does not carry them. Drop them, or run the sweep with --no-detach (#1359)")
	}
	script, err := buildUserDataSeeded(plat, cfg, publicKey, storageScript, readyMountPoints, object)
	if err != nil {
		return "", err
	}
	ud := encodeUserDataForOS(script, cfg.TargetOS)
	if err := aws.CheckUserDataSize(ud); err != nil {
		return "", err
	}
	return ud, nil
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
