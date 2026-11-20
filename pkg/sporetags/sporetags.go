// Package sporetags is how spawn puts the spore.host tags from
// github.com/spore-host/libs/tags on what it creates (spawn#1652, part of the
// leave-no-trace epic spore-host/spore-host#752), so that cleanup can find
// every resource spawn made and nothing it did not.
//
// # The keys
//
// Every resource spawn creates carries spore-host:managed-by=spawn,
// spore-host:created, spore-host:schema and, where known, spore-host:scope
// (the region for a regional resource, [ScopeGlobal] for IAM) and
// spore-host:owner (the creating principal's aws:userid). They sit beside the
// spawn:* tags, which are unchanged: the reaper, `spawn cleanup` and spored
// key on those, and TTL enforcement keeps keying on them.
//
// # Precedence: the caller's spore-host:* keys win
//
// spawn is also a library: calque and lagotto launch instances and create
// instance roles through pkg/aws, and pass their own
// spore-host:managed-by=calque (or lagotto) because they, not spawn, own those
// resources' lifecycle. spawn therefore writes each spore-host:* key only when
// the caller did not supply it ([Fill]). spawn:* keys stay spawn's own: a
// caller's spawn:* value never replaces spawn's (the --tag help reserves the
// prefix).
//
// # No spore-host:role
//
// spawn never writes spore-host:role here. Its one value, "service", marks
// spore.host's own live service (pkg/servicetags, spore-host#750), which
// cleanup refuses to delete; what this package tags is workload and tool
// infrastructure that must stay deletable. The two resources kept on purpose,
// the permanent account setup `spawn setup-account` provisions (the
// spawn-instance permissions boundary and the user lifecycle policy), carry
// [PermanentKey] instead.
package sporetags

import (
	"time"

	"github.com/spore-host/libs/tags"
)

// ScopeGlobal is the spore-host:scope of an IAM resource, which belongs to no
// region.
const ScopeGlobal = "global"

// PermanentKey marks the spawn resources kept on purpose by owner decision
// (spore-host/spore-host#752): the permanent account setup, exactly two
// policies, spore-host/spawn-instance-boundary and
// spore-host/spawn-user-lifecycle, removed only by `spawn setup-account
// --remove`. Teardown keys on it and leaves the resource in place. Its value is
// [PermanentValue].
const (
	PermanentKey   = "spore-host:permanent"
	PermanentValue = "true"
)

// Now is the clock the spore-host:created tag is stamped from; a variable so
// tests can pin it.
var Now = time.Now

// For is the spore.host tag set for a resource spawn creates now in scope, for
// owner. An empty scope or owner is left out.
func For(scope, owner string) map[string]string {
	return tags.For(tags.ToolSpawn, scope, Now(), owner)
}

// At is [For] stamped at now instead of the current time.
func At(scope, owner string, now time.Time) map[string]string {
	return tags.For(tags.ToolSpawn, scope, now, owner)
}

// Permanent is [For] plus [PermanentKey]: the tags of the spawn-instance
// boundary and the user lifecycle policy.
func Permanent(owner string) map[string]string {
	m := For(ScopeGlobal, owner)
	m[PermanentKey] = PermanentValue
	return m
}

// Fill returns a new map holding dst and, for every key of add that dst does
// not have, add's value: the caller's keys win. Neither argument is modified.
// This is the precedence rule for a caller-supplied tag map.
func Fill(dst, add map[string]string) map[string]string {
	out := make(map[string]string, len(dst)+len(add))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range add {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// Converge returns the tags to write so that an existing resource, carrying
// existing, also carries desired. It only adds: a key the resource already has
// keeps its value, so a re-run never moves spore-host:created (cleanup's grace
// clock) or spore-host:owner, and never relabels a resource another tool's
// spore-host:managed-by claims. A resource with every desired key yields nil,
// so the caller can skip the write. existing may be nil.
func Converge(existing, desired map[string]string) map[string]string {
	var out map[string]string
	for k, v := range desired {
		if _, ok := existing[k]; ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}
