package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/sweep"
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

// sweepCrossAccountRoleName is the role the sweep orchestrator assumes in the
// account the members launch in.
const sweepCrossAccountRoleName = sweep.CrossAccountRoleName

// sweepRoleIAM builds the IAM client requireSweepCrossAccountRole asks, with
// the credentials of the account the members launch in. A var so the hermetic
// tests can count what reaches it.
var sweepRoleIAM = sweep.NewRoleGetter

// requireSweepCrossAccountRole refuses a detached sweep in an account without
// SpawnSweepCrossAccountRole, before the sweep creates anything (#1535).
//
// The orchestrator launches every member through that role. Without it, the
// CLI used to create the CLI IAM role, the instance profile, the egress
// proxies, the params upload and the orchestration record, invoke the
// orchestrator, and report "queued successfully"; the orchestrator then failed
// to assume the role and launched nothing. The CLI cannot test the assume
// itself (the role trusts only the orchestrator's role), so it checks the
// role is there, with iam:GetRole, which spawn launch already calls.
//
// Only a definite absence refuses. Any other error (no iam:GetRole, a
// throttle) is reported and the sweep goes on, as it did before this check:
// a missing permission to look is not evidence the role is missing.
func requireSweepCrossAccountRole(ctx context.Context, c sweep.RoleGetter, accountID string, warn func(string)) error {
	err := sweep.CheckCrossAccountRole(ctx, c)
	if err == nil {
		return nil
	}
	if errors.Is(err, sweep.ErrCrossAccountRoleMissing) {
		return fmt.Errorf("a detached sweep needs %s in account %s: the sweep orchestrator launches every member "+
			"through it, and this account has none, so it could launch nothing. Nothing was created. "+
			"Run the sweep with --no-detach to launch the members from this CLI instead, or use an account "+
			"set up for detached sweeps", sweepCrossAccountRoleName, accountID)
	}
	warn(fmt.Sprintf("could not confirm %s exists in account %s (%v); the sweep orchestrator needs it to launch the members",
		sweepCrossAccountRoleName, accountID, err))
	return nil
}

// requireDetachedSweepRole is requireSweepCrossAccountRole for the account a
// detached sweep's members launch in: the compute account, as
// launchSweepDetached resolves it. recordAccount is the account a resumed
// sweep's record names ("" at launch); when the credentials are for another
// account, the role there says nothing about this sweep, so nothing is
// checked.
func requireDetachedSweepRole(ctx context.Context, recordAccount string) error {
	cfg, err := spawnconfig.LoadComputeAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	accountID, err := aws.NewClientFromConfig(cfg).GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get AWS account ID: %w", err)
	}
	warn := func(msg string) { fmt.Fprintf(os.Stderr, "⚠️  %s\n", msg) }
	if recordAccount != "" && recordAccount != accountID {
		warn(fmt.Sprintf("not checking %s: the sweep's members launch in account %s, and these credentials are for %s",
			sweepCrossAccountRoleName, recordAccount, accountID))
		return nil
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // IAM is global; the SDK still wants a region to sign for
	}
	return requireSweepCrossAccountRole(ctx, sweepRoleIAM(cfg), accountID, warn)
}
