package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/spore-host/libs/lifecycle"

	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/reaperconfig"
	"github.com/spore-host/spawn/pkg/reaperdeploy"
)

// The self-hosted reaper's removal of itself (spawn#1655, part of the
// leave-no-trace epic spore-host/spore-host#752).
//
// Only the reaper `spawn reaper deploy` made (spawn-ttl-reaper-selfscan,
// scanning only its own account: selfHostedFrom) removes itself. Never the
// operated reaper in spore.host's infra account, nor one a SAM stack made:
// those are the service, or a stack's to delete.
//
// When: the account-idle sweep (accountidle.go) has, in one run, claimed,
// re-checked and reported EVERY scope it judges (each scanned region and
// global; a scope no creator has marked counts only when nothing in it is
// active), and the artifact bucket holds nothing but ttl-reaper/ objects. To
// make "every scope in one run" reachable, a scope that comes due while
// another is still inside its grace, with none active, is held until they all
// are (holdForSelfRemoval): each report releases its lease and restarts that
// scope's grace, so scopes reported in different runs would stay out of
// phase for good.
//
// What: reaperdeploy.PlanSelfRemoval. One-shot Scheduler jobs, run as
// spawn-reaper-teardown-invoke (made at deploy), delete the function at +2m,
// empty the role at +4m and delete it at +5m (kept while another region's
// reaper shares it), and delete the log group at +15m; the reaper itself
// empties and deletes the artifact bucket (only when tagged spawn:managed)
// and, last, its rules, their targets and its invoke permissions.
//
// REPORT MODE ONLY. Every step is logged as
//
//	reaper-selfremoval REPORT step=<i>/<n> action=<what> how=<direct|one-shot|kept> offset=+<d> target=<api> resource=<arn>
//
// with a summary line, and nothing is created or deleted.
// REAPER_SELF_REMOVAL=reap is accepted and reports, saying so, until
// spawn#1657 (effectiveSelfRemovalMode, pinned by a test).
const (
	selfRemovalOff    = reaperconfig.ModeOff
	selfRemovalReport = reaperconfig.ModeReport
	selfRemovalReap   = reaperconfig.ModeReap
)

// parseSelfRemoval maps REAPER_SELF_REMOVAL onto a mode: unset and anything
// unrecognised are report.
func parseSelfRemoval(v string) string {
	if m, ok := reaperconfig.ParseMode(v); ok {
		return m
	}
	return selfRemovalReport
}

// selfRemovalModeFromEnv reads REAPER_SELF_REMOVAL, saying so when reap was
// asked for.
func selfRemovalModeFromEnv() string {
	m := parseSelfRemoval(os.Getenv("REAPER_SELF_REMOVAL"))
	if m == selfRemovalReap {
		log.Printf("REAPER_SELF_REMOVAL=reap: the reaper cannot remove itself yet (spawn#1657); it reports")
	}
	return m
}

// effectiveSelfRemovalMode is the mode the self-removal acts in. Reap
// reports until spawn#1657 arms it; a test pins this.
func effectiveSelfRemovalMode(m string) string {
	if m == selfRemovalReap {
		return selfRemovalReport
	}
	return m
}

// selfHosted is the reaper's own identity, set only for the reaper `spawn
// reaper deploy` made.
type selfHosted struct {
	accountID, region string
	// deployer plans the removal (and, from spawn#1657, creates its jobs)
	// over the reaper's own credentials, in its own region.
	deployer *reaperdeploy.Deployer
	// lambdaIn is a Lambda client in another region, for the check that
	// no reaper there shares the role.
	lambdaIn func(region string) reaperdeploy.LambdaAPI
}

// selfHostedFrom is the reaper's identity when it is `spawn reaper deploy`'s:
// its function name is reaperdeploy.FunctionName, it scans its own account
// (REAPER_SCAN_SELF=true) and only that one. Anything else is nil.
func selfHostedFrom(cfg aws.Config, accounts []account) *selfHosted {
	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != reaperdeploy.FunctionName ||
		!strings.EqualFold(os.Getenv("REAPER_SCAN_SELF"), "true") ||
		len(parseList(os.Getenv("REAPER_ROLE_ARNS"))) > 0 || strings.TrimSpace(os.Getenv("EC2_ROLE_ARN")) != "" ||
		len(accounts) != 1 || accounts[0].label != "self" || accounts[0].accountID == "" || cfg.Region == "" {
		return nil
	}
	return &selfHosted{
		accountID: accounts[0].accountID,
		region:    cfg.Region,
		deployer:  reaperdeploy.New(cfg),
		lambdaIn: func(region string) reaperdeploy.LambdaAPI {
			return lambda.NewFromConfig(regionalCopy(cfg, region))
		},
	}
}

// selfRemovalApplies reports whether this run may plan the reaper's own
// removal for acct: the reaper is self-hosted, acct is its own account, and
// REAPER_SELF_REMOVAL is not off.
func (r *reaper) selfRemovalApplies(acct account) bool {
	return r.self != nil && r.selfRemoval != selfRemovalOff && r.selfRemoval != "" && acct.accountID == r.self.accountID
}

// Per-scope judgments the account-idle sweep makes before it claims anything.
const (
	scopeUnavailable = iota // the lease could not be read or observed
	scopeUnmarked           // no spawn creator has marked it: never torn down
	scopeActive
	scopeWaiting // idle, inside the grace
	scopeDue     // idle past the grace
)

// scopeJudgment is one scope's observation in a run.
type scopeJudgment struct {
	scope   string
	reasons []string
	state   int
	idleFor time.Duration
}

// holdForSelfRemoval reports which due scopes wait for the others, so that
// every scope comes due in one run and the self-removal can be planned. Only
// when nothing is active or unreadable (activity anywhere means the account
// is not idle, and the per-scope reports go on as before) and some scope is
// still inside its grace (it will come due within the grace, unless activity
// starts, which lifts the hold).
func holdForSelfRemoval(js []scopeJudgment) (held bool, waiting []string) {
	for _, j := range js {
		switch {
		case j.state == scopeUnavailable, j.state == scopeActive,
			j.state == scopeUnmarked && len(j.reasons) > 0:
			return false, nil
		case j.state == scopeWaiting:
			waiting = append(waiting, j.scope)
		}
	}
	return len(waiting) > 0, waiting
}

// selfRemovalBlocker says why the self-removal is not planned this run, given
// the scopes reported: "" when every scope was reported, or has never been
// marked and is idle.
func selfRemovalBlocker(js []scopeJudgment, reported map[string]bool) string {
	var why []string
	for _, j := range js {
		switch {
		case reported[j.scope]:
		case j.state == scopeUnmarked && len(j.reasons) == 0:
		case j.state == scopeUnmarked:
			why = append(why, j.scope+" is active")
		case j.state == scopeUnavailable:
			why = append(why, j.scope+"'s lease could not be read")
		case j.state == scopeActive:
			why = append(why, j.scope+" is active")
		case j.state == scopeWaiting:
			why = append(why, j.scope+" is inside its grace")
		default:
			why = append(why, j.scope+" was not reported this run")
		}
	}
	return strings.Join(why, "; ")
}

// removeSelf plans the self-hosted reaper's removal and logs it, under the
// global scope's lease. Report mode: it creates nothing and deletes nothing.
func (r *reaper) removeSelf(ctx context.Context, acct account, lease *lifecycle.Lease, now time.Time, sum *Summary) {
	s := r.self
	in := reaperdeploy.SelfRemovalInput{AccountID: s.accountID, Region: s.region, Now: now, LambdaIn: s.lambdaIn}
	if acct.regionsFor == nil {
		in.RegionsErr = errors.New("no way to list the account's enabled regions")
	} else {
		in.Regions, in.RegionsErr = instancerole.EnabledRegions(ctx, acct.regionsFor(), s.region)
	}
	plan, err := s.deployer.PlanSelfRemoval(ctx, in)
	if err != nil {
		sum.SelfRemovalBlocked++
		log.Printf("reaper-selfremoval: account=%s region=%s: not planned: %v", s.accountID, s.region, err)
		return
	}
	mode := effectiveSelfRemovalMode(r.selfRemoval)
	label := strings.ToUpper(mode)
	for i, st := range plan.Steps {
		line := fmt.Sprintf("reaper-selfremoval %s step=%d/%d action=%s how=%s offset=+%s target=%s resource=%s",
			label, i+1, len(plan.Steps), st.Step, st.How, st.Offset, st.Target, st.Resource)
		if j := st.Job; j != nil {
			line += fmt.Sprintf(" name=%s group=%s at=%s input=%s", j.Name, reaperdeploy.TeardownScheduleGroup, j.At.UTC().Format(time.RFC3339), j.Input)
		}
		if st.Reason != "" {
			line += fmt.Sprintf(" reason=%q", st.Reason)
		}
		log.Printf("%s", line)
	}
	if mode == selfRemovalReap {
		// spawn#1657. Unreachable while effectiveSelfRemovalMode maps reap to
		// report: the one-shot jobs first, so a failure leaves the rules
		// invoking a reaper whose next run creates the same jobs again.
		if _, err := reaperdeploy.CreateOneShots(ctx, s.deployer.Scheduler, s.region, s.accountID, plan.Version,
			plan.OneShots(), lease.Step); err != nil {
			log.Printf("reaper-selfremoval: account=%s region=%s: the one-shot jobs could not all be created: %v", s.accountID, s.region, err)
			return
		}
	}
	sum.SelfRemovalPlanned++
	sum.SelfRemovalOneShots += len(plan.OneShots())
	log.Printf("reaper-selfremoval %s account=%s region=%s steps=%d one_shots=%d direct=%d kept=%d group=%s invoke_role=%s role_kept=%q",
		label, s.accountID, s.region, len(plan.Steps), len(plan.OneShots()), plan.Count(reaperdeploy.HowDirect), plan.Count(reaperdeploy.HowKept),
		reaperdeploy.TeardownScheduleGroup, reaperdeploy.TeardownInvokeRoleARN(s.accountID), plan.RoleKept)
}
