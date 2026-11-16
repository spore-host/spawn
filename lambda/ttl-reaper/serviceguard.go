package main

import (
	"errors"
	"fmt"

	"github.com/spore-host/spawn/pkg/servicetags"
)

// The infra-account guard (spore-host/spore-host#750, part of the leave-no-trace
// epic spore-host/spore-host#752).
//
// The infra account holds spore.host's own service: the dashboard, the
// orchestrators, the DNS updater, this reaper. Most of it carries
// spawn:managed=true, the same tag spawn puts on workload litter, and an
// automated litter classifier marked 12 of those live resources as litter. A
// sweep that judges "idle" or "orphaned" by spawn's tags therefore cannot be
// trusted there. Two rules, both enforced here:
//
//   - no idle teardown and no account-wide deletion runs against the infra
//     account at all, nor against an account the reaper could not identify,
//     which might be it;
//   - no resource tagged spore-host:role=service is ever deleted, in any
//     account.
//
// The idle sweep itself (spawn#1654) does not exist yet; these are the checks it
// must call. The existing orphan sweeps call them already: the net-resource,
// empty-bucket, per-task role and pool-queue sweeps run GuardTeardown before
// they list anything, and check each resource with GuardDelete or, inside
// pkg/bucketsweep, pkg/instancerole and pkg/poolqueue, the same
// servicetags.IsService keep rule.

// InfraAccountID is spore.host's shared infra account (docs/infra-account.md).
const InfraAccountID = "966362334030"

// Why a teardown or a deletion was refused. Wrapped with the account or the
// resource, so errors.Is matches them.
var (
	ErrInfraAccount    = errors.New("refusing teardown in spore.host's infra account")
	ErrAccountUnknown  = errors.New("refusing teardown in an account the reaper could not identify")
	ErrServiceResource = errors.New("refusing to delete a resource tagged spore-host:role=service")
)

// GuardTeardown returns nil only when an idle teardown or an account-wide
// deletion may run in accountID. It refuses the infra account, and an empty
// account ID: the reaper's own account is resolved through STS, and a failed
// lookup leaves it empty, so "unknown" may well be the infra account.
func GuardTeardown(accountID string) error {
	switch accountID {
	case "":
		return ErrAccountUnknown
	case InfraAccountID:
		return fmt.Errorf("%w (%s)", ErrInfraAccount, accountID)
	}
	return nil
}

// GuardDelete returns nil only when the resource named what, in accountID and
// carrying tags, may be deleted by a teardown or sweep: GuardTeardown allows
// the account, and the resource is not tagged spore-host:role=service.
func GuardDelete(accountID, what string, tags map[string]string) error {
	if err := GuardTeardown(accountID); err != nil {
		return err
	}
	if servicetags.IsService(tags) {
		return fmt.Errorf("%w: %s (spore-host:component=%q)", ErrServiceResource, what, tags[servicetags.ComponentKey])
	}
	return nil
}
