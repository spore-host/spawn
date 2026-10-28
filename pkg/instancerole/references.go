package instancerole

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Deferred launchers (#1378).
//
// "No instance holds the profile and IAM has not seen the role used for a
// day" means a role is no longer needed only if nothing will launch with it
// later. A schedule does: `spawn schedule create` accepts a spawn-instance-*
// profile (refuseUnpassableDetachedProfiles, #1331), stores its name in the
// schedule's params, and the sweep orchestrator passes that name to
// RunInstances at every firing, without re-creating the role. A schedule that
// fires weekly leaves its role idle between firings, so without this the
// sweep would delete it and the next firing would fail every member.
//
// So a sweep takes References: every profile name a deferred launcher will
// use, collected from its ReferenceSources right before the sweep. A role
// named there is kept (KeepReferencedPrefix). And the collection fails closed:
// if any source cannot be read completely, CollectReferences returns an error,
// and Sweep refuses to run without References, so no role is deleted.
//
// The deferred launchers spawn has, and which of them need a source:
//
//   - schedules: ScheduleSource. The only one that holds a profile name across
//     an idle gap with nothing running.
//   - detached sweeps and pipelines in flight: their orchestrators launch the
//     next member or stage within one invocation (13 minutes) of the last one
//     finishing, and the last one held the profile and refreshed RoleLastUsed,
//     so neither can leave the role unused for Grace while it still has
//     something to launch.
//   - lagotto watches: the watcher creates the role (CreateOrGetInstanceProfile)
//     when it launches, so a deleted one is re-created.
//   - pools and batch queues: their workers hold the profile while they run.

// KeepReferencedPrefix opens the reason a role named by a deferred launcher is
// kept: "referenced by schedule sched-20261028-120000-ab12cd".
const KeepReferencedPrefix = "referenced by "

// ReferenceSource is one kind of deferred launcher: it lists every instance
// profile name it will launch with later.
type ReferenceSource interface {
	// Describe names the source for messages, e.g. "schedules (spawn-schedules
	// in us-east-1)".
	Describe() string
	// ProfileReferences maps each profile name to what names it ("schedule
	// <id>"). An error means the answer is incomplete, and nothing may be
	// deleted.
	ProfileReferences(ctx context.Context) (map[string][]string, error)
}

// References is what CollectReferences found. Its zero value is "not
// collected", which Sweep refuses, so a caller that forgets to collect
// deletes nothing rather than everything.
type References struct {
	by        map[string][]string
	collected bool
	sources   []string
}

// For returns what references the profile name, sorted; nil if nothing does.
func (r References) For(name string) []string {
	return r.by[name]
}

// Sources are the sources the references were collected from.
func (r References) Sources() []string { return r.sources }

// ReferencesError is CollectReferences' failure: Source could not be read.
type ReferencesError struct {
	Source string
	Err    error
}

func (e *ReferencesError) Error() string {
	return fmt.Sprintf("could not read %s, so no role can be shown to be unreferenced: %v", e.Source, e.Err)
}

func (e *ReferencesError) Unwrap() error { return e.Err }

// ErrReferencesNotCollected is Sweep's refusal to run with References that
// CollectReferences did not produce.
var ErrReferencesNotCollected = errors.New("the deferred-launch references (schedules) were not collected, so no role can be shown to be unreferenced")

// CollectReferences reads every source. It fails, and the caller must delete
// nothing, if there are no sources (a discovery that found nothing has broken)
// or any source fails.
func CollectReferences(ctx context.Context, sources ...ReferenceSource) (References, error) {
	if len(sources) == 0 {
		return References{}, errors.New("no deferred-launch reference sources configured, so no role can be shown to be unreferenced")
	}
	refs := References{by: map[string][]string{}, collected: true}
	for _, src := range sources {
		if src == nil {
			return References{}, errors.New("a nil deferred-launch reference source")
		}
		got, err := src.ProfileReferences(ctx)
		if err != nil {
			return References{}, &ReferencesError{Source: src.Describe(), Err: err}
		}
		for name, by := range got {
			n := normalizeProfileName(name)
			if n == "" {
				continue
			}
			refs.by[n] = append(refs.by[n], by...)
		}
		refs.sources = append(refs.sources, src.Describe())
	}
	for n, by := range refs.by {
		sort.Strings(by)
		refs.by[n] = dedupSorted(by)
	}
	return refs, nil
}

// normalizeProfileName reduces an instance profile reference to the name IAM
// knows it by: an ARN's last path element, trimmed.
func normalizeProfileName(s string) string {
	return strings.TrimSpace(ProfileNameFromARN(strings.TrimSpace(s)))
}

func dedupSorted(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
