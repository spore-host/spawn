// Failure classification and the alarmable signals built on it (#469, #457
// failure mode A, #254 idea 3).
//
// The reaper had exactly one way to describe a failure — sum.Errors++ — and
// nothing consumed it. handler always returns a nil error, so Lambda's own Errors
// metric never moved; the Slack webhook only ever fired on successful reaps; and
// no metric filter or alarm existed on this function at all. So a region whose
// scan failed every run for weeks was indistinguishable, from the outside, from a
// region with nothing to reap.
//
// That is the dangerous direction. Errors is the ONLY field in Summary that says
// a scan did not happen — every other field counts something that did. An
// instance running past its deadline in a region we cannot see looks exactly like
// a clean run, which is precisely the blindness #65 created the reaper to
// prevent.
//
// Two things were wrong and they compound:
//
//  1. Every failure was the same failure. A deleted cross-account role (a
//     configuration fact, recurring every run until a human edits something) and a
//     throttle (an operational fact that clears itself) both incremented the same
//     counter. The permanent one then buries the transient one.
//
//  2. Failures were counted per region. Credentials are per ACCOUNT, so one
//     uninstalled customer produced 11 identical errors per run — 66/hour, forever,
//     pinning the "investigate this" field at a number that can never return to
//     zero without a human editing a deploy parameter. That is how a signal gets
//     trained out of an operator.
//
// The classification here is deliberately coarse: denied vs operational. Finer
// grades would invite acting on them, and acting is exactly what this file must
// not do. Nothing here deletes, terminates, or drops an account from the scan
// set — it only decides how to COUNT and what to SAY.
package main

import (
	"errors"
	"fmt"
	"log"

	"github.com/aws/smithy-go"
)

// Sentinel log lines. These exist to be alarmed on, and their exact text is a
// contract with the metric filters in template.yaml — CloudWatch matches the
// literal string, so changing one here without changing the template silently
// disarms the alarm. Tests assert both spellings for that reason.
//
// The prober learned this the same way (see its refused-correlated-failure
// filter): a refusal that is correct but SILENT is indistinguishable from a run
// with nothing to do.
const (
	// sentinelAllAccountsFailed means we reached ZERO accounts this run. The
	// wording points at us on purpose. The reaper's Lambda role ARN embeds a
	// CloudFormation-generated physical ID, so recreating the stack changes that
	// suffix and breaks EVERY customer's trust policy simultaneously (#457 trap 1)
	// — which looks, from the inside, exactly like the entire customer base
	// uninstalling at once. The first suspect is always our own deploy.
	sentinelAllAccountsFailed = "REAPER REACHED NO ACCOUNTS"

	// sentinelAccountDenied means one account's role refused us in every region.
	// Almost certainly standing misconfiguration — a deleted role, a trust policy
	// that no longer names us, or a changed EC2_EXTERNAL_ID. It is NOT proof the
	// customer left: STS answers AccessDenied both for a deleted role and for a
	// role whose trust policy excludes us, deliberately, so as not to leak whether
	// the role exists.
	sentinelAccountDenied = "REAPER ACCOUNT UNREACHABLE"

	// sentinelFSxDenied means the EC2 scan worked for an account but every FSx
	// DescribeFileSystems call was refused. This is #212 exactly: an FSx
	// AccessDenied that surfaced as a silent no-op because the code path was
	// best-effort and under-logged (#254's recurring class). It needs its own
	// sentinel rather than folding into sentinelAccountDenied — fsx:* is a
	// different grant from ec2:*, so an account can be perfectly reachable and
	// still never have a filesystem reclaimed. Sharing the EC2 signal would let a
	// successful EC2 scan mask this entirely, which is how #212 stayed invisible.
	sentinelFSxDenied = "REAPER FSX UNREACHABLE"
)

// scanFailure is how a single AWS call failed, coarsely. The distinction is
// load-bearing: one of these will still be failing next run no matter what we do,
// and the other probably will not.
type scanFailure int

const (
	// failureNone — no error.
	failureNone scanFailure = iota
	// failureDenied — an authorization refusal. A CONFIGURATION fact: it recurs
	// every run until a human changes something, so counting it as an operational
	// error means the operational error count is never zero again.
	failureDenied
	// failureOperational — throttling, timeout, a service blip, anything else. An
	// OPERATIONAL fact: transient by default, and the thing "investigate this"
	// should actually mean.
	failureOperational
)

func (f scanFailure) String() string {
	switch f {
	case failureDenied:
		return "denied"
	case failureOperational:
		return "operational"
	default:
		return "none"
	}
}

// classifyScanError sorts an AWS error into the two classes above.
//
// The error-code set is taken verbatim from the prober's isDenied
// (spore-host/lambda/portal-account-prober/probe.go) rather than re-derived. That
// function is the one already trusted to make this exact call against the same
// STS/EC2 APIs, and two copies of a security-relevant code list drift.
//
// Kept pure — no AWS, no clock, no logging — for the same reason as
// authorizeExpiry: the interesting behaviour is a refusal to over-interpret, and
// a refusal is only trustworthy if it can be tested exhaustively.
func classifyScanError(err error) scanFailure {
	if err == nil {
		return failureNone
	}
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		// Not an AWS API error at all (context deadline, DNS, a wrapped local
		// failure). Operational by default: unknown failures must land in the class
		// that gets LOUDER, never in the one that gets quieter.
		return failureOperational
	}
	switch ae.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation":
		return failureDenied
	default:
		return failureOperational
	}
}

// accountOutcome accumulates one account's results across every region, because
// per-region counting is what made a single dead account look like eleven
// problems. Credentials are per account: the same role either works everywhere or
// is refused everywhere, so eleven identical AccessDenieds are ONE observation.
type accountOutcome struct {
	label     string
	accountID string // for the registry's second opinion, which is keyed by ID not label
	regions   int    // regions attempted
	reached   int    // regions that answered
	denied    int    // regions that refused us
	opErrors  int    // regions that failed for any other reason

	// FSx is tracked separately because fsx:* is a different grant from ec2:*: an
	// account can be fully reachable for instances and refuse every filesystem
	// call. That is #212. Folding the two together would let the EC2 success mask
	// the FSx denial, which is precisely how #212 went unnoticed.
	fsxRegions int
	fsxReached int
	fsxDenied  int
}

// record folds one region's instance-scan result in.
func (o *accountOutcome) record(err error) {
	o.regions++
	switch classifyScanError(err) {
	case failureNone:
		o.reached++
	case failureDenied:
		o.denied++
	default:
		o.opErrors++
	}
}

// recordFSx folds one region's FSx result in. Operational FSx failures still
// increment Errors at the call site (unchanged behaviour); only denials are
// re-routed here, so this tracks just enough to tell "refused everywhere" from
// "refused once".
func (o *accountOutcome) recordFSx(err error) {
	o.fsxRegions++
	switch classifyScanError(err) {
	case failureNone:
		o.fsxReached++
	case failureDenied:
		o.fsxDenied++
	}
}

// fsxDeniedEverywhere reports whether every FSx call for this account was refused
// and none succeeded.
func (o *accountOutcome) fsxDeniedEverywhere() bool {
	return o.fsxRegions > 0 && o.fsxReached == 0 && o.fsxDenied > 0
}

// deniedEverywhere reports whether this account refused us in every region it was
// attempted in, and answered in none.
//
// Requiring reached == 0 matters: an account that answers in ten regions and
// denies in one is not an unreachable account, it is an opt-in region we lack
// authorization for (a plain us-east-1-only role hitting an enabled-by-default
// region). Reporting that as "unreachable" would send an operator hunting for a
// deleted role that is fine.
func (o *accountOutcome) deniedEverywhere() bool {
	return o.regions > 0 && o.reached == 0 && o.denied > 0
}

// unreachable reports whether the account answered in NO region, for any reason.
// Used only for the correlated-failure guard, which asks "did our probing work at
// all" and does not care why it did not.
func (o *accountOutcome) unreachable() bool {
	return o.regions > 0 && o.reached == 0
}

// errorsToCount is what this account contributes to Summary.Errors.
//
// A denied account contributes ZERO, and that is the whole point of #469: the
// field means "something unexpected happened, investigate it", and a permanent
// expected failure sitting in it forever is what trains an operator to stop
// reading it. The denial is not discarded — it is counted in AccountsDenied and
// announced with its own sentinel, so it gets LOUDER, not quieter.
//
// Operational failures are still counted per region, deliberately: unlike
// credentials, they really are independent per region, and three throttled
// regions are three events worth seeing.
func (o *accountOutcome) errorsToCount() int {
	return o.opErrors
}

// reportOutcomes logs the alarmable conclusions for a whole run and folds the
// per-account totals into the Summary.
//
// registryStatus is an optional lookup supplying the portal registry's lifecycle
// verdict for an account ID. It is CORROBORATION ONLY and can never gate
// anything, because it describes a DIFFERENT ROLE: REAPER_ROLE_ARNS holds
// spawn-ttl-reaper-ec2 (which trusts this Lambda), while the registry holds
// spore-portal-onboard (which trusts portal-phone-home). An account can be
// healthy in one and absent from the other in both directions, so the registry
// saying "unreachable" is a second opinion about a different door — useful in the
// log, never authoritative.
func reportOutcomes(outcomes []accountOutcome, sum *Summary, registryStatus func(string) string) {
	if len(outcomes) == 0 {
		return
	}

	reachedAny := false
	for i := range outcomes {
		o := &outcomes[i]
		sum.Errors += o.errorsToCount()
		if !o.unreachable() {
			reachedAny = true
		}
		// The FSx signal is independent of the EC2 one and must be reported even for
		// an account whose instance scan succeeded — that combination IS #212.
		if o.fsxDeniedEverywhere() {
			sum.FSxAccountsDenied++
			log.Printf("%s: account %s refused every FSx call in all %d region(s) — the role's fsx:DescribeFileSystems grant is missing, so orphaned filesystems are accruing cost unreclaimed and unseen. The instance scan for this account %s, which is why this needs its own signal (#212, #254)%s",
				sentinelFSxDenied, o.label, o.fsxRegions,
				map[bool]string{true: "SUCCEEDED", false: "also failed"}[o.reached > 0],
				registryHint(o.accountID, registryStatus))
		}

		if !o.deniedEverywhere() {
			continue
		}
		sum.AccountsDenied++
		log.Printf("%s: account %s refused us in all %d region(s) — the role is gone, or its trust policy no longer names this reaper, or EC2_EXTERNAL_ID changed. NOT counted in errors (#469)%s",
			sentinelAccountDenied, o.label, o.regions, registryHint(o.accountID, registryStatus))
	}

	// Correlated-failure guard, mirroring accountlifecycle.ApplyProbes' central
	// safety rule and the DNS sweep's refusal to delete against a partial live set:
	// an observation that could be explained by our own breakage is not evidence
	// about the customer. Unlike those two, this one cannot refuse to act — the
	// reaper's job is to terminate, and it simply found nothing to terminate. All
	// it can do is say so, loudly, which is what this line is for.
	if !reachedAny {
		log.Printf("%s: all %d account(s) failed this run — investigate OUR side first: this Lambda's execution role, the cross-account trust policies (a recreated reaper stack changes the role ARN suffix and breaks every one at once), or EC2_EXTERNAL_ID. No instance was reaped, and that may mean over-deadline instances are running unseen (#457 trap 1, #469)",
			sentinelAllAccountsFailed, len(outcomes))
	}
}

// registryHint renders the registry's second opinion for a log line, or "" when
// there is nothing to say. Absence is reported explicitly rather than silently:
// "the registry has no row for this account" and "the registry was unreadable"
// are different facts from "the registry agrees", and a blank line would let the
// reader assume the reassuring one.
func registryHint(accountID string, registryStatus func(string) string) string {
	if registryStatus == nil {
		return ""
	}
	switch status := registryStatus(accountID); status {
	case "":
		return " [portal registry: no row for this account — no second opinion available]"
	default:
		return fmt.Sprintf(" [portal registry: status %q — corroboration only, it describes the spore-portal-onboard role, not this one]", status)
	}
}
