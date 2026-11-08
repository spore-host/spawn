package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// Which orchestration path a parameter sweep takes (#1535).
//
// The path used to be re-derived at each place that needed it, from the
// --detach, --no-detach and --max-concurrent globals, and the derivations
// disagreed: the auto-detach branch set detach and a max-concurrent, but an
// explicit --detach skipped that branch, left max-concurrent at 0 and so
// failed the `detach && maxConcurrent > 0` dispatch condition. That sweep ran
// in the foreground while the summary printed "Orchestration: Lambda
// (detached)", --wait was accepted for it, and the detached-only refusals
// were applied to it.
//
// sweepDispatch decides the path once, and everything that depends on it
// (the refusals, --wait, the summary, the dispatch) reads its decision.
// TestSweepDispatchIsTheOnlyReaderOfTheDetachFlags keeps it that way.

// sweepPath is the orchestration path a parameter sweep takes.
type sweepPath int

const (
	// sweepForeground: this CLI launches every member and must stay running
	// (--no-detach).
	sweepForeground sweepPath = iota
	// sweepDetached: the sweep orchestrator launches the members from the
	// orchestration record (the default, and --detach).
	sweepDetached
)

func (p sweepPath) String() string {
	if p == sweepDetached {
		return "detached"
	}
	return "foreground"
}

// detachedDefaultMaxConcurrent caps a detached sweep that names no
// --max-concurrent: min(rows, 10) members at a time. The orchestrator launches
// only while active < max_concurrent, so a detached sweep must never be
// recorded with 0, which would launch nothing.
const detachedDefaultMaxConcurrent = 10

// sweepDispatchDecision is sweepDispatch's answer.
type sweepDispatchDecision struct {
	path sweepPath
	// maxConcurrent is the effective --max-concurrent. 0 only on the
	// foreground path, where it means all at once; on the detached path it is
	// always >= 1 once deriveAuto is resolved.
	maxConcurrent int
	// autoDetached: no detach flag was given, so the sweep is detached by
	// default.
	autoDetached bool
	// defaulted: maxConcurrent is detachedDefaultMaxConcurrent's rule, not a
	// value the user gave.
	defaulted bool
	// deriveAuto: --max-concurrent-auto derives maxConcurrent later, from the
	// account's quota (it needs AWS). withDerived fills it in.
	deriveAuto bool
}

// sweepDispatch decides a parameter sweep's orchestration path and effective
// max-concurrent from the flags and the row count. It is the only place the
// --detach/--no-detach/--max-concurrent globals are interpreted (#1535).
//
//	--no-detach              foreground; max-concurrent as given (0 = all at once)
//	(no flag) or --detach    detached; max-concurrent as given, else
//	                         --max-concurrent-auto's, else min(rows, 10)
func sweepDispatch(detach, noDetach bool, maxConcurrent int, maxConcurrentAuto bool, rows int) (sweepDispatchDecision, error) {
	if detach && noDetach {
		return sweepDispatchDecision{}, errors.New("--detach and --no-detach are mutually exclusive")
	}
	if maxConcurrentAuto && maxConcurrent > 0 {
		return sweepDispatchDecision{}, errors.New("--max-concurrent-auto and --max-concurrent are mutually exclusive — pick one")
	}
	if maxConcurrent < 0 {
		return sweepDispatchDecision{}, fmt.Errorf("--max-concurrent %d: want 0 or more", maxConcurrent)
	}
	if noDetach {
		return sweepDispatchDecision{path: sweepForeground, maxConcurrent: maxConcurrent, deriveAuto: maxConcurrentAuto}, nil
	}
	d := sweepDispatchDecision{path: sweepDetached, autoDetached: !detach, maxConcurrent: maxConcurrent, deriveAuto: maxConcurrentAuto}
	if maxConcurrent == 0 && !maxConcurrentAuto {
		d.maxConcurrent = min(max(rows, 1), detachedDefaultMaxConcurrent)
		d.defaulted = true
	}
	return d, nil
}

// withDerived is d with --max-concurrent-auto's derived value in place.
func (d sweepDispatchDecision) withDerived(n int) sweepDispatchDecision {
	d.maxConcurrent = n
	d.deriveAuto = false
	return d
}

// orchestrationLine is the summary's "Orchestration:" value, from the path
// the sweep takes. The foreground line names no orchestrator, because there is
// none.
func (d sweepDispatchDecision) orchestrationLine() string {
	if d.path == sweepDetached {
		return "Lambda (detached)"
	}
	return "foreground (--no-detach: this CLI launches every member; keep it running)"
}

// refuseWaitOnPath refuses --wait on a sweep that is not detached: it waits
// for the orchestration record, which only a detached sweep has.
func refuseWaitOnPath(wait bool, d sweepDispatchDecision) error {
	if wait && d.path != sweepDetached {
		return errors.New("--wait waits for a detached sweep, and --no-detach runs this one in the foreground " +
			"(the launch already waits for every member): drop one of them")
	}
	return nil
}

// memberAccountLine is the line a detached sweep prints, before it creates
// anything, naming the account its members launch in and where that came
// from (#1538). recordAccount is a resumed sweep's recorded account ("" at
// launch): the orchestrator launches there, whatever these credentials are.
func memberAccountLine(computeAccount, recordAccount string, cp spawnconfig.ComputeProfile) string {
	if recordAccount != "" && recordAccount != computeAccount {
		return fmt.Sprintf("🏦 Members launch in account %s (the sweep's record); the compute credentials are for account %s (%s)",
			recordAccount, computeAccount, cp.Describe())
	}
	return fmt.Sprintf("🏦 Members launch in account %s (%s)", computeAccount, cp.Describe())
}

// newComputeClient is a client for the compute account
// (spawnconfig.LoadComputeAWSConfig), in region, else the shared default
// region, as spawnaws.NewClientWithRegion chooses one. What a detached
// sweep's members depend on is created with it, in the account they launch
// in (#1538). A variable so tests can see which account a path uses.
var newComputeClient = func(ctx context.Context, region string) (*aws.Client, error) {
	if region == "" {
		region = spawnconfig.SharedRegion()
	}
	cfg, err := spawnconfig.LoadComputeAWSConfig(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("failed to load compute AWS config: %w", err)
	}
	return aws.NewClientFromConfig(cfg), nil
}
