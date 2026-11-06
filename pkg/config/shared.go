package config

import (
	"os"
	"sync"

	"github.com/spore-host/libs/sporeconfig"
)

// Shared spore.host config base (libs/sporeconfig). The CLI pushes its
// --profile/--region/--account flag values in via SetSharedFlags during
// PersistentPreRun; the config layer then resolves the shared Config with
// flag > env (SPORE_*/AWS_*) > ~/.config/spore/config.toml > default.
//
// spawn keeps its infra/compute two-account model on top of this. Infra
// (GetInfraProfile) is spore.host's control-plane account by design: it wins
// via SPAWN_INFRA_PROFILE and its spore-host-infra default, and falls through
// to the shared profile only when explicitly cleared (SPAWN_INFRA_PROFILE="").
// Compute (ResolveComputeProfile) is the caller's own account unless
// SPAWN_COMPUTE_PROFILE or compute_profile in ~/.spawn/config.yaml names
// another (#1538); it has no hardcoded default.

var (
	sharedMu    sync.RWMutex
	sharedFlags sporeconfig.Flags
)

// SetSharedFlags records the CLI flag values for the shared config resolver.
// Called once from the root command's PersistentPreRun. Empty fields are
// treated as unset and fall through to env/file/default.
func SetSharedFlags(profile, region, account string) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	sharedFlags = sporeconfig.Flags{Profile: profile, Region: region, Account: account}
}

// SharedConfig resolves the shared spore.host config (profile/region/account/
// output) using the recorded flags plus env/file/default. A malformed shared
// config file is tolerated: the returned Config is still populated from the
// flag/env/default layers (the error is discarded here so config resolution
// never hard-fails on an advisory file).
func SharedConfig() sporeconfig.Config {
	sharedMu.RLock()
	flags := sharedFlags
	sharedMu.RUnlock()
	cfg, _ := sporeconfig.Resolve(flags)
	return cfg
}

// SharedProfile returns the shared AWS profile (or "" for the ambient chain).
func SharedProfile() string { return SharedConfig().Profile }

// SharedRegion returns the shared default AWS region (or "" for ambient).
func SharedRegion() string { return SharedConfig().Region }

// SharedProfileSource returns the shared AWS profile and the layer that set
// it, with the shared config's precedence: "--profile", "SPORE_PROFILE",
// "AWS_PROFILE" or the spore config file. It returns "", "" for the ambient
// chain.
func SharedProfileSource() (profile, source string) {
	profile = SharedProfile()
	if profile == "" {
		return "", ""
	}
	sharedMu.RLock()
	flag := sharedFlags.Profile
	sharedMu.RUnlock()
	switch {
	case flag != "":
		return profile, "--profile"
	case os.Getenv("SPORE_PROFILE") != "":
		return profile, "SPORE_PROFILE"
	case os.Getenv("AWS_PROFILE") != "":
		return profile, "AWS_PROFILE"
	default:
		return profile, "profile in the spore config file"
	}
}
