package aws

import (
	"errors"
	"fmt"
	"strings"
)

// The nested-spawn role may call RunInstances and iam:PassRole on
// spawn-instance-*, so an instance carrying it can start more instances. `spawn
// launch --nested-spawn` therefore requires --ttl (#39). Naming the role
// directly, by --instance-profile, a sweep row's iam_role or a LaunchConfig
// built by an SDK caller, attaches exactly the same capability, so it is held to
// exactly the same rule (#1375). CheckNestedSpawnTTL is that rule, and every
// path that accepts a profile calls it (or refuses the role outright); Client.Launch
// and launcher.Provision call it too, so no caller of the library escapes it.

// ErrNestedSpawnNeedsTTL is the reason a nested-spawn launch without a TTL is
// refused. Every refusal wraps it, so callers and tests can match it with
// errors.Is whatever named the role.
var ErrNestedSpawnNeedsTTL = errors.New("an instance that can launch other instances must have a hard lifetime")

// ErrNestedSpawnRoleReserved is the refusal for a path that would create, edit or
// pass a nested-spawn role where nested spawn makes no sense: --iam-role (whose
// role spawn creates or rewrites, which would replace the nested role's fixed,
// audited grants), a batch queue, or a detached sweep.
var ErrNestedSpawnRoleReserved = errors.New("the " + NestedSpawnRolePrefix + "* roles are reserved for --nested-spawn")

// NestedSpawnTTLError is the refusal of --nested-spawn itself without --ttl.
// The flag path and the named-role path share its wording.
func NestedSpawnTTLError() error {
	return fmt.Errorf("--nested-spawn requires --ttl: %w", ErrNestedSpawnNeedsTTL)
}

// CheckNestedSpawnTTL refuses profile when it is a nested-spawn role
// (IsNestedSpawnRole) and ttl is empty. source says where the profile came from
// ("--instance-profile", "param set 3's iam_role:"). Any other profile, or a
// nested one with a TTL, returns nil. No AWS call.
//
// A TTL is required, not merely a bound: an idle timeout stops rather than
// terminates by default, and a head node busy dispatching may never be idle.
func CheckNestedSpawnTTL(source, profile, ttl string) error {
	if !IsNestedSpawnRole(profile) || strings.TrimSpace(ttl) != "" {
		return nil
	}
	return fmt.Errorf("%s %q is a nested-spawn role (%s*), which, like --nested-spawn, requires --ttl: %w",
		source, profile, NestedSpawnRolePrefix, ErrNestedSpawnNeedsTTL)
}

// ValidateNestedSpawnTTL applies CheckNestedSpawnTTL to the config's own
// instance profile and TTL.
func (c LaunchConfig) ValidateNestedSpawnTTL() error {
	return CheckNestedSpawnTTL("instance profile", c.IamInstanceProfile, c.TTL)
}

// NestedSpawnRoleReservedError is the outright refusal of a nested-spawn role
// named by source where it cannot be used; why completes the sentence.
func NestedSpawnRoleReservedError(source, role, why string) error {
	return fmt.Errorf("%s %q: %w: %s; use `spawn launch --nested-spawn --ttl <duration>` for an instance that launches others",
		source, role, ErrNestedSpawnRoleReserved, why)
}
