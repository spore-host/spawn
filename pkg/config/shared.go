package config

import (
	"sync"

	"github.com/spore-host/libs/sporeconfig"
)

// Shared spore.host config base (libs/sporeconfig). The CLI pushes its
// --profile/--region/--account flag values in via SetSharedFlags during
// PersistentPreRun; the config layer then resolves the shared Config with
// flag > env (SPORE_*/AWS_*) > ~/.config/spore/config.toml > default.
//
// spawn keeps its infra/compute two-account model (GetInfraProfile /
// GetComputeProfile) on top of this: those still win via SPAWN_INFRA_PROFILE /
// SPAWN_COMPUTE_PROFILE and their spore-host-* defaults, and only fall through
// to the shared profile when a caller explicitly clears the spawn default
// (SPAWN_*_PROFILE="") — see GetInfraProfile/GetComputeProfile.

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
