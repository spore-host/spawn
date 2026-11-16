// Package servicetags names the tags that mark a resource as part of
// spore.host's own long-lived service, the control plane in the infra account,
// as opposed to something a tool created for a user (spore-host/spore-host#750).
//
// The infra account's service resources mostly carry spawn:managed=true, which
// is also the tag spawn puts on workload litter, so a litter classifier could
// not tell the two apart and marked 12 live resources as litter. Every service
// deploy path now adds three more tags, and cleanup refuses anything carrying
// the first:
//
//	spore-host:role=service
//	spore-host:component=<id>        dashboard-api, ttl-reaper, ...
//	spore-host:deployed-by=<path>    the repo-relative deploy mechanism
//
// spore-host:role and spore-host:component come from libs/tags. libs has no
// constant for spore-host:deployed-by yet, so it is defined here. The shell
// deploy scripts read the same three keys from scripts/lib/service-tags.sh and
// the CI deployer from scripts/deploylambdas/servicetags.go;
// TestServiceTagKeysAgree keeps the three in step.
package servicetags

import "github.com/spore-host/libs/tags"

// The tag keys and the one value with a fixed meaning.
const (
	// RoleKey is spore-host:role.
	RoleKey = tags.Role
	// RoleService is the RoleKey value on a service resource.
	RoleService = tags.RoleService
	// ComponentKey is spore-host:component: which part of the service the
	// resource belongs to (dashboard-api, ttl-reaper, ...).
	ComponentKey = tags.Component
	// DeployedByKey is spore-host:deployed-by: the repo-relative path of the
	// mechanism that deploys the resource, so an inventory can say where to
	// look before anything touches it.
	DeployedByKey = "spore-host:deployed-by"
)

// For returns the three service tags for a resource of component deployed by
// deployedBy. The map is new; the caller may add to it.
func For(component, deployedBy string) map[string]string {
	return map[string]string{
		RoleKey:       RoleService,
		ComponentKey:  component,
		DeployedByKey: deployedBy,
	}
}

// IsService reports whether a resource's tags mark it as part of the service.
// Cleanup must never delete such a resource.
func IsService(t map[string]string) bool {
	return t[RoleKey] == RoleService
}
