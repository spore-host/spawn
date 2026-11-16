package main

import (
	"path"
	"sort"
	"strings"
)

// The spore-host:* service tags (spore-host/spore-host#750), which every service
// deploy path puts on what it deploys so cleanup can tell spore.host's own
// service from workload litter: both carry spawn:managed=true.
//
// This module has no dependencies, so it cannot import libs/tags or
// pkg/servicetags; the keys are written out here instead.
// pkg/servicetags.TestServiceTagKeysAgree fails if they drift from that package
// or from scripts/lib/service-tags.sh.
const (
	serviceRoleKey       = "spore-host:role"
	serviceRoleValue     = "service"
	serviceComponentKey  = "spore-host:component"
	serviceDeployedByKey = "spore-host:deployed-by"
)

// deployerPath is spore-host:deployed-by for a function whose checklist entry
// names no mechanism, which the checklist never produces today.
const deployerPath = "scripts/deploylambdas"

// serviceTags is the set a function of l must carry: the component is the
// Lambda directory's name (lambda/ttl-reaper -> ttl-reaper), and deployed-by
// is the component's own deploy mechanism, since that is where its definition
// lives; this job only ships code.
func serviceTags(l checklistLambda) map[string]string {
	deployedBy := deployerPath
	if len(l.Mechanisms) > 0 {
		deployedBy = l.Mechanisms[0].Path
	}
	return map[string]string{
		serviceRoleKey:       serviceRoleValue,
		serviceComponentKey:  path.Base(l.ID),
		serviceDeployedByKey: deployedBy,
	}
}

// missingServiceTags is what of want the live tags lack. Role and component are
// written whenever they differ. deployed-by only when absent: a function with
// two mechanisms (dashboard-api) carries whichever deployed it last, and this
// job, which only ships code, must not make the two disagree on every run.
func missingServiceTags(live, want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range want {
		cur, ok := live[k]
		if k == serviceDeployedByKey && ok && cur != "" {
			continue
		}
		if cur != v {
			out[k] = v
		}
	}
	return out
}

// tagList renders tags as the --tags shorthand, sorted so the call is
// reproducible.
func tagList(tags map[string]string) string {
	kv := make([]string, 0, len(tags))
	for k, v := range tags {
		kv = append(kv, k+"="+v)
	}
	sort.Strings(kv)
	return strings.Join(kv, ",")
}
