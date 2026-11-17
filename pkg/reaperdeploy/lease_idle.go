package reaperdeploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"

	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/reaperconfig"
)

// NewLeaseReader is the lifecycle client `spawn reaper teardown
// --if-idle-for` reads the verdict with, over cfg (the caller's credentials):
// the same tables, and the same control region for the global scope, as
// spawn's creators write (pkg/lifecyclemark).
func NewLeaseReader(cfg awssdk.Config) (LeaseReader, error) {
	return lifecycle.New(lifecycle.Options{
		DynamoDB: func(region string) (lifecycle.API, error) {
			c := cfg.Copy()
			c.Region = region
			return dynamodb.NewFromConfig(c), nil
		},
		ControlRegion: lifecyclemark.ControlRegion,
		Owner:         "spawn-reaper-teardown",
	})
}

// LeaseReader is the one lifecycle read the teardown idle check makes. A
// *lifecycle.Client satisfies it.
type LeaseReader interface {
	Get(ctx context.Context, tool tags.Tool, scope string) (lifecycle.Item, bool, error)
}

// AccountIdleness is what the lifecycle verdict says about whether spawn is
// idle in this account (spawn#1654), for `spawn reaper teardown
// --if-idle-for`.
//
// The verdict is the in-account reaper's account-idle sweep: every run it
// reads every activity signal (instances pending through stopped, egress
// proxies, FSx, pool queues, calque) and records ACTIVE or IDLE, with the
// time it first saw the scope idle, in the account's spore-host-lifecycle
// table. A launch marks the scope active there itself (pkg/lifecyclemark).
// This only reads it. It replaces judging idleness by the absence of action
// words in the reaper's logs, which said "the reaper had nothing to reap",
// not "nothing spawn manages is in use".
type AccountIdleness struct {
	// Determined is false when a scope's verdict could not be read or does
	// not exist; nothing may then be removed.
	Determined bool
	// Why explains an undetermined result.
	Why string
	// Active names the first scope the verdict says is active, and why.
	Active string
	// IdleFor is the shortest idleness over every scope.
	IdleFor time.Duration
}

// LeaseIdleness reads spawn's lifecycle verdict for each of regions and the
// global scope (IAM).
//
// A scope with no item, or one no spawn creator has ever marked active
// (Epoch 0), has no verdict: the lease is unavailable there (this account's
// users cannot write the table, or the reaper's sweep is off) and idleness is
// undetermined, never assumed.
func LeaseIdleness(ctx context.Context, lr LeaseReader, regions []string, now time.Time) AccountIdleness {
	if lr == nil {
		return AccountIdleness{Why: "no lifecycle client"}
	}
	scopes := append(append([]string{}, regions...), lifecycle.ScopeGlobal)
	out := AccountIdleness{Determined: true}
	first := true
	for _, scope := range scopes {
		it, ok, err := lr.Get(ctx, tags.ToolSpawn, scope)
		switch {
		case err != nil:
			return AccountIdleness{Why: fmt.Sprintf("the lifecycle verdict for %s could not be read: %v", scope, err)}
		case !ok || it.Epoch == 0:
			return AccountIdleness{Why: fmt.Sprintf("no lifecycle verdict for %s in %s (lease unavailable: no spawn launch has marked it, "+
				"or the reaper's account-idle sweep is off)", scope, lifecycle.TableName)}
		}
		switch it.State {
		case lifecycle.StateActive:
			if out.Active == "" {
				out.Active = scope + " is active"
			}
			out.IdleFor, first = 0, false
			continue
		case lifecycle.StateIdle, lifecycle.StateTeardown:
		default:
			return AccountIdleness{Why: fmt.Sprintf("the lifecycle verdict for %s is %q", scope, it.State)}
		}
		d := it.IdleFor(now)
		if first || d < out.IdleFor {
			out.IdleFor = d
		}
		first = false
	}
	return out
}

// RegionsOf is the reaper's scanned regions from its REAPER_REGIONS value,
// or fallback when it has none.
func RegionsOf(v, fallback string) []string {
	var out []string
	for _, r := range strings.Split(v, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 && fallback != "" {
		out = []string{fallback}
	}
	return out
}

// IdleTeardownRefusal decides `spawn reaper teardown --if-idle-for want`. It
// returns why the reaper must not be torn down, or "" when it may.
//
// Idleness is the reaper's own lifecycle verdict (spawn#1654,
// LeaseIdleness), read through lr; lerr is why lr could not be built. It used
// to be the absence of action words in the reaper's logs, which only ever said
// the reaper had had nothing to reap. The logs are still read for one thing:
// that the reaper RUNS (DetectIdleness). A reaper that does not run has
// recorded no verdict worth trusting, and removing it would delete something
// that never got the chance to work.
//
// The refusals, every one a scheduled no-op: idleness undetermined (logs,
// settings, or the lifecycle table unreadable; a scope with no verdict); the
// reaper not running; its account-idle sweep off; want longer than the
// sweep's grace, which its verdict never exceeds (each report releases the
// lease, and Lease.Release restarts the grace); spawn active in any scope; or
// idle for less than want.
func (d *Deployer) IdleTeardownRefusal(ctx context.Context, lr LeaseReader, lerr error, region string, want time.Duration, now time.Time) string {
	idle := d.DetectIdleness(ctx, now)
	switch {
	case !idle.Determined:
		return fmt.Sprintf("could not determine idleness (%s)", idle.Why)
	case !idle.Ran:
		return "the reaper has not run at all, which is a broken schedule rather than an idle one. Check `spawn reaper status`"
	}
	info, err := d.Inspect(ctx)
	if err != nil {
		return fmt.Sprintf("could not determine idleness (the reaper's settings: %v)", err)
	}
	if info.AccountIdle == reaperconfig.ModeOff {
		return "the reaper's account-idle sweep is off (REAPER_ACCOUNT_IDLE=off), so there is no lifecycle verdict to judge idleness by"
	}
	if want > info.AccountIdleGrace {
		return fmt.Sprintf("--if-idle-for %s is longer than the reaper's account-idle grace (%s), the longest idleness its verdict records; "+
			"redeploy with 'spawn reaper deploy --account-idle-grace %s' to wait that long", want, info.AccountIdleGrace, want)
	}
	if lerr != nil {
		return fmt.Sprintf("could not determine idleness (the lifecycle table: %v)", lerr)
	}
	v := LeaseIdleness(ctx, lr, RegionsOf(info.Regions, region), now)
	switch {
	case !v.Determined:
		return fmt.Sprintf("could not determine idleness (%s)", v.Why)
	case v.Active != "":
		return fmt.Sprintf("spawn is active in this account (%s, by the reaper's lifecycle verdict)", v.Active)
	case v.IdleFor < want:
		return fmt.Sprintf("idle for %s, which is less than the %s required", v.IdleFor.Round(time.Minute), want)
	}
	return ""
}
