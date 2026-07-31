package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spore-host/lambda/accountlifecycle"
)

// apiErr builds a smithy APIError with a given code, which is how the AWS SDK
// surfaces AccessDenied and friends through errors.As.
func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code + " (test)"}
}

// TestClassifyScanError covers every code the reaper must sort, plus the shapes
// that are NOT AWS errors at all.
//
// The default matters more than the matches: an unrecognized failure must land in
// failureOperational, the class that still increments Errors and still gets
// investigated. Defaulting the other way would let an unfamiliar authorization
// code silently stop counting, which is the exact failure this change exists to
// remove.
func TestClassifyScanError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want scanFailure
		why  string
	}{
		{"nil", nil, failureNone, "no error is not a failure"},
		{"AccessDenied", apiErr("AccessDenied"), failureDenied,
			"STS's answer for both a deleted role and an untrusting trust policy"},
		{"AccessDeniedException", apiErr("AccessDeniedException"), failureDenied,
			"the same refusal, spelled the way the newer services spell it"},
		{"UnauthorizedOperation", apiErr("UnauthorizedOperation"), failureDenied,
			"EC2's spelling of the same refusal"},
		{"Throttling", apiErr("Throttling"), failureOperational,
			"transient: clears on its own, so it is what 'investigate this' should mean"},
		{"RequestLimitExceeded", apiErr("RequestLimitExceeded"), failureOperational,
			"transient"},
		{"ServiceUnavailable", apiErr("ServiceUnavailable"), failureOperational,
			"an outage is operational, not configuration"},
		{"ExpiredToken", apiErr("ExpiredToken"), failureOperational,
			"not in the denial set: it is our own credential lifecycle, and must stay loud"},
		{"InvalidClientTokenId", apiErr("InvalidClientTokenId"), failureOperational,
			"a broken principal on OUR side — must stay loud, never quiesced as a customer fact"},
		{"plain error", errors.New("dial tcp: i/o timeout"), failureOperational,
			"not an API error at all; unknown failures must get louder, never quieter"},
		{"wrapped denial", fmt.Errorf("assume role: %w", apiErr("AccessDenied")), failureDenied,
			"errors.As must see through wrapping — the SDK wraps in operation errors"},
	}

	for _, tc := range cases {
		if got := classifyScanError(tc.err); got != tc.want {
			t.Errorf("classifyScanError(%s) = %v, want %v — %s", tc.name, got, tc.want, tc.why)
		}
	}
}

// The classifier's String() feeds a log line an operator reads, so an empty or
// wrong rendering is a real (if small) defect.
func TestScanFailureString(t *testing.T) {
	for f, want := range map[scanFailure]string{
		failureNone:        "none",
		failureDenied:      "denied",
		failureOperational: "operational",
	} {
		if got := f.String(); got != want {
			t.Errorf("scanFailure(%d).String() = %q, want %q", f, got, want)
		}
	}
}

// denyAll builds an outcome for an account refused in every region — the dead- or
// misconfigured-account case that motivated #469.
func denyAll(label string, regions int) accountOutcome {
	o := accountOutcome{label: label, accountID: "111111111111"}
	for i := 0; i < regions; i++ {
		o.record(apiErr("AccessDenied"))
	}
	return o
}

// Plan case 1: denied in every region contributes AccountsDenied 1 — not 11 — and
// leaves Errors untouched.
//
// The 11 is the whole point. 11 regions × 6 runs/hour = 66 errors/hour, forever,
// for one uninstalled customer. A field that can never return to zero without a
// human editing a deploy parameter has stopped being a signal.
func TestReportOutcomes_DeniedEverywhereCountsOnceNotPerRegion(t *testing.T) {
	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{denyAll("acct-a", 11)}, &sum, nil)
	})

	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1 — one account's credentials are ONE observation, not %d", sum.AccountsDenied, 11)
	}
	if sum.Errors != 0 {
		t.Errorf("errors = %d, want 0 — a standing authorization refusal must not pin the 'investigate this' field (#469)", sum.Errors)
	}
	if !strings.Contains(out, sentinelAccountDenied) {
		t.Errorf("must log the %q sentinel — it is the only thing an alarm can match; got:\n%s", sentinelAccountDenied, out)
	}
}

// Plan case 2: a throttle in one region still counts as an error, and must NOT be
// reported as an unreachable account. Quieting this would be the change doing real
// harm: it is the class of failure that actually warrants a look.
func TestReportOutcomes_OperationalFailureStillCountsAndIsNotADenial(t *testing.T) {
	o := accountOutcome{label: "acct-a"}
	o.record(nil)
	o.record(apiErr("Throttling"))
	o.record(nil)

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, nil)
	})

	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1 — operational failures are exactly what this field is for", sum.Errors)
	}
	if sum.AccountsDenied != 0 {
		t.Errorf("accounts_denied = %d, want 0 — a throttle is not a misconfigured role", sum.AccountsDenied)
	}
	if strings.Contains(out, sentinelAccountDenied) || strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("no sentinel may fire for a single throttled region — a sentinel that cries wolf is worse than none; got:\n%s", out)
	}
}

// A denial in ONE region while others answer is not an unreachable account — it is
// most likely an opt-in region the role has no authorization for. Reporting it as
// unreachable would send an operator hunting a deleted role that is fine.
func TestReportOutcomes_DeniedInOneRegionOnlyIsNotAnUnreachableAccount(t *testing.T) {
	o := accountOutcome{label: "acct-a"}
	o.record(nil)
	o.record(apiErr("AccessDenied"))
	o.record(nil)

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, nil)
	})

	if sum.AccountsDenied != 0 {
		t.Errorf("accounts_denied = %d, want 0 — the account answered in 2 of 3 regions, so its role is not gone", sum.AccountsDenied)
	}
	if strings.Contains(out, sentinelAccountDenied) {
		t.Errorf("must not claim the account is unreachable when it answered; got:\n%s", out)
	}
}

// Plan case 3: one denied account alongside a healthy one. The per-account split
// must hold, and — critically — the healthy account must not be swept up in the
// other's failure.
func TestReportOutcomes_MixedHealthyAndDenied(t *testing.T) {
	healthy := accountOutcome{label: "acct-healthy"}
	healthy.record(nil)
	healthy.record(nil)

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{healthy, denyAll("acct-dead", 11)}, &sum, nil)
	})

	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1", sum.AccountsDenied)
	}
	if sum.Errors != 0 {
		t.Errorf("errors = %d, want 0", sum.Errors)
	}
	if strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("the correlated-failure sentinel must NOT fire while any account was reached — otherwise it stops meaning 'our side is broken'; got:\n%s", out)
	}
	if !strings.Contains(out, "acct-dead") {
		t.Errorf("the denied account must be named; got:\n%s", out)
	}
	if strings.Contains(out, "acct-healthy") {
		t.Errorf("the healthy account must not appear in a failure line; got:\n%s", out)
	}
}

// Plan case 4: EVERY account failed. This is the signal that matters most, and it
// must point at US.
//
// #457 trap 1: the reaper's role ARN embeds a CloudFormation-generated physical
// ID, so recreating the stack changes the suffix and breaks every customer's trust
// policy at once — indistinguishable, from the inside, from the entire customer
// base uninstalling simultaneously. An operator who reads this line and starts
// emailing customers has been actively misled, so the wording is asserted.
func TestReportOutcomes_AllAccountsFailedFiresCorrelatedSentinel(t *testing.T) {
	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{denyAll("a", 3), denyAll("b", 3)}, &sum, nil)
	})

	if !strings.Contains(out, sentinelAllAccountsFailed) {
		t.Fatalf("must log the %q sentinel; got:\n%s", sentinelAllAccountsFailed, out)
	}
	if !strings.Contains(out, "OUR side") {
		t.Errorf("the sentinel must direct the operator at our own deploy first, not at the customers; got:\n%s", out)
	}
}

// The correlated sentinel must fire for OPERATIONAL total failure too. A
// region-wide EC2 outage or an expired execution-role credential reaches zero
// accounts just as effectively as a broken trust policy, and the consequence — no
// instance reaped, over-deadline instances running unseen — is identical.
func TestReportOutcomes_AllAccountsFailedOperationallyAlsoFires(t *testing.T) {
	o := accountOutcome{label: "acct-a"}
	o.record(apiErr("ServiceUnavailable"))
	o.record(apiErr("ServiceUnavailable"))

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, nil)
	})

	if !strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("reaching zero accounts must alarm regardless of WHY; got:\n%s", out)
	}
	if sum.Errors != 2 {
		t.Errorf("errors = %d, want 2 — operational failures are per-region because they really are independent", sum.Errors)
	}
	if sum.AccountsDenied != 0 {
		t.Errorf("accounts_denied = %d, want 0 — nothing refused us", sum.AccountsDenied)
	}
}

// A clean run must be silent. Without this, a sentinel could fire on every run and
// the alarm would be permanently useless — the same way Errors was.
func TestReportOutcomes_HealthyRunLogsNoSentinel(t *testing.T) {
	o := accountOutcome{label: "acct-a"}
	o.record(nil)
	o.record(nil)

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, nil)
	})

	if out != "" {
		t.Errorf("a healthy run must log nothing here; got:\n%s", out)
	}
	if sum.Errors != 0 || sum.AccountsDenied != 0 {
		t.Errorf("healthy run produced errors=%d denied=%d, want 0/0", sum.Errors, sum.AccountsDenied)
	}
}

// A run with no accounts configured must not claim "all accounts failed". Zero of
// zero is not a failure, and firing here would alarm on a misconfiguration the
// startup path already reports.
func TestReportOutcomes_NoAccountsIsNotACorrelatedFailure(t *testing.T) {
	var sum Summary
	out := captureLogs(func() {
		reportOutcomes(nil, &sum, nil)
	})
	if out != "" {
		t.Errorf("zero configured accounts must not fire a sentinel; got:\n%s", out)
	}
}

// Plan case 5a: a denied account WITH a registry row gets the second opinion
// printed — and the log must say it is only corroboration.
//
// The registry describes spore-portal-onboard; the denial we just observed is
// about spawn-ttl-reaper-ec2. Two different roles, either of which can be healthy
// while the other is broken. An operator who reads "unreachable" here and treats
// it as confirmation that the customer is gone has drawn a conclusion the data
// does not support, so the disclaimer is asserted as behaviour.
func TestReportOutcomes_RegistryRowIsAnnotatedAsCorroborationOnly(t *testing.T) {
	o := denyAll("acct-dead", 3)
	o.accountID = "222222222222"
	lookup := func(id string) string {
		if id == "222222222222" {
			return accountlifecycle.StatusUnreachable
		}
		return ""
	}

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, lookup)
	})

	if !strings.Contains(out, accountlifecycle.StatusUnreachable) {
		t.Errorf("the registry's verdict must appear; got:\n%s", out)
	}
	if !strings.Contains(out, "corroboration only") {
		t.Errorf("the log must say the registry is not authoritative — it describes a DIFFERENT role; got:\n%s", out)
	}
	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1 — the registry annotates, it never changes the count", sum.AccountsDenied)
	}
}

// Plan case 5b: a denied account with NO registry row is still counted, and the
// absence is stated. Saying nothing would let the reader assume the reassuring
// reading ("the registry agrees"); "there is no second opinion" is a different
// fact and the more useful one.
func TestReportOutcomes_MissingRegistryRowSaysSoAndStillCounts(t *testing.T) {
	o := denyAll("acct-dead", 3)
	o.accountID = "333333333333"

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, func(string) string { return "" })
	})

	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1 — a missing registry row must never suppress our own observation", sum.AccountsDenied)
	}
	if !strings.Contains(out, "no row for this account") {
		t.Errorf("must state that no second opinion exists; got:\n%s", out)
	}
}

// Plan case 6: the registry being unreadable must not break classification and
// must not gate anything. registryStatusFunc returns nil in that case, so
// reportOutcomes simply says nothing about the registry.
func TestReportOutcomes_NilRegistryLookupIsHarmless(t *testing.T) {
	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{denyAll("acct-dead", 3)}, &sum, nil)
	})

	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1 — our own AccessDenied stands on its own", sum.AccountsDenied)
	}
	if strings.Contains(out, "portal registry") {
		t.Errorf("with no registry configured the log must not mention one; got:\n%s", out)
	}
}

// registryStatusFunc must read the registry AT MOST once per run, and not at all
// when no account was denied. The Scan is a real cost against a table that exists
// for another purpose entirely; paying it on every healthy run would be a
// regression the Summary would never reveal.
func TestRegistryStatusFunc_ReadsLazilyAndOnce(t *testing.T) {
	reg := &countingRegistry{rows: []accountlifecycle.Account{row("444444444444", accountlifecycle.StatusDormant)}}
	rp := &reaper{registry: reg}
	var sum Summary
	lookup := rp.registryStatusFunc(context.Background(), &sum)

	if reg.calls != 0 {
		t.Fatalf("registry read %d times before any lookup, want 0 — a healthy run must not pay for the Scan", reg.calls)
	}
	if got := lookup("444444444444"); got != accountlifecycle.StatusDormant {
		t.Errorf("lookup = %q, want %q", got, accountlifecycle.StatusDormant)
	}
	lookup("555555555555")
	lookup("444444444444")
	if reg.calls != 1 {
		t.Errorf("registry read %d times, want 1 — three lookups in one run must share a single Scan", reg.calls)
	}
}

// An unreadable registry yields empty verdicts rather than a crash or a fabricated
// status, and the read failure is itself counted so a registry that has been
// broken for weeks is visible.
func TestRegistryStatusFunc_UnreadableRegistryYieldsNoVerdict(t *testing.T) {
	rp := &reaper{registry: &fakeRegistry{err: errors.New("dynamodb unavailable")}}
	var sum Summary
	captureLogs(func() {
		lookup := rp.registryStatusFunc(context.Background(), &sum)
		if got := lookup("666666666666"); got != "" {
			t.Errorf("lookup = %q, want empty — an unreadable registry has no opinion", got)
		}
	})
	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1 — an unreadable registry is an operational failure worth seeing", sum.Errors)
	}
}

// With no registry configured at all, the lookup is nil — the report path then
// omits registry commentary entirely rather than printing a misleading blank.
func TestRegistryStatusFunc_NoRegistryReturnsNil(t *testing.T) {
	rp := &reaper{}
	var sum Summary
	if rp.registryStatusFunc(context.Background(), &sum) != nil {
		t.Error("want nil lookup when no registry is configured")
	}
}

// An account present in the registry but with a status the reaper does not
// recognize must render that status verbatim, not silently blank. Blank means "no
// row", and conflating the two hides a registry the prober is writing badly.
func TestRegistryStatusFunc_UnknownStatusIsRenderedVerbatim(t *testing.T) {
	reg := &countingRegistry{rows: []accountlifecycle.Account{row("777777777777", "some-future-status")}}
	rp := &reaper{registry: reg}
	var sum Summary
	lookup := rp.registryStatusFunc(context.Background(), &sum)
	if got := lookup("777777777777"); got != "some-future-status" {
		t.Errorf("lookup = %q, want the status verbatim — blank would be indistinguishable from 'no row'", got)
	}
}

// The FSx signal must be independent of the EC2 one. This is #212: an FSx
// AccessDenied that surfaced as a silent no-op. fsx:* is a separate grant, so an
// account can scan instances perfectly and never reclaim a filesystem — and if the
// two shared one signal, the EC2 success would mask it exactly as it did then.
func TestReportOutcomes_FSxDeniedIsReportedEvenWhenEC2Succeeded(t *testing.T) {
	o := accountOutcome{label: "acct-a"}
	o.record(nil)
	o.record(nil)
	o.recordFSx(apiErr("AccessDenied"))
	o.recordFSx(apiErr("AccessDenied"))

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, nil)
	})

	if sum.FSxAccountsDenied != 1 {
		t.Errorf("fsx_accounts_denied = %d, want 1 — a healthy EC2 scan must not mask a missing fsx:* grant (#212)", sum.FSxAccountsDenied)
	}
	if !strings.Contains(out, sentinelFSxDenied) {
		t.Errorf("must log the %q sentinel; got:\n%s", sentinelFSxDenied, out)
	}
	if !strings.Contains(out, "SUCCEEDED") {
		t.Errorf("the log must note that the instance scan worked — that contrast is what identifies it as a grant problem, not a dead account; got:\n%s", out)
	}
	if sum.AccountsDenied != 0 {
		t.Errorf("accounts_denied = %d, want 0 — the account itself was reachable", sum.AccountsDenied)
	}
	if sum.Errors != 0 {
		t.Errorf("errors = %d, want 0 — a standing FSx denial is counted in its own field, not this one", sum.Errors)
	}
}

// An FSx call that succeeds anywhere means the grant exists, so a single refused
// region is not a missing grant.
func TestReportOutcomes_FSxDeniedInOneRegionOnlyIsNotAGrantProblem(t *testing.T) {
	o := accountOutcome{label: "acct-a"}
	o.record(nil)
	o.recordFSx(nil)
	o.recordFSx(apiErr("AccessDenied"))

	var sum Summary
	out := captureLogs(func() {
		reportOutcomes([]accountOutcome{o}, &sum, nil)
	})

	if sum.FSxAccountsDenied != 0 {
		t.Errorf("fsx_accounts_denied = %d, want 0 — FSx answered in another region, so the grant is present", sum.FSxAccountsDenied)
	}
	if strings.Contains(out, sentinelFSxDenied) {
		t.Errorf("must not fire the FSx sentinel; got:\n%s", out)
	}
}

// An operational FSx failure must not be recorded as a denial. recordFSx tracks
// only reachability, so an outage leaves fsxDeniedEverywhere false and the error
// keeps counting at the call site.
func TestAccountOutcome_FSxOperationalFailureIsNotADenial(t *testing.T) {
	o := accountOutcome{label: "acct-a"}
	o.recordFSx(apiErr("ServiceUnavailable"))

	if o.fsxDeniedEverywhere() {
		t.Error("an FSx outage must not be reported as a missing fsx:* grant — it needs the loud path, not the quiet one")
	}
}

// The sentinel strings are a CONTRACT with the metric filters in template.yaml:
// CloudWatch matches the literal text, so renaming one here without editing the
// template silently disarms the alarm and nothing else in the build would notice.
// Pinning the exact spellings makes that a test failure instead.
func TestSentinelSpellingsAreStable(t *testing.T) {
	for got, want := range map[string]string{
		sentinelAllAccountsFailed: "REAPER REACHED NO ACCOUNTS",
		sentinelAccountDenied:     "REAPER ACCOUNT UNREACHABLE",
		sentinelFSxDenied:         "REAPER FSX UNREACHABLE",
	} {
		if got != want {
			t.Errorf("sentinel = %q, want %q — update the MetricFilter patterns in template.yaml in the same commit", got, want)
		}
	}
}

// countingRegistry counts ListAccounts calls, so the laziness of the registry read
// is observable.
type countingRegistry struct {
	rows  []accountlifecycle.Account
	calls int
}

func (c *countingRegistry) ListAccounts(ctx context.Context) ([]accountlifecycle.Account, error) {
	c.calls++
	return c.rows, nil
}

// ---------------------------------------------------------------------------
// End-to-end wiring, driven through handler with failing AWS clients.
//
// These exist because mutation testing found the pure tests above could not
// notice the feature being DISCONNECTED: removing the reportOutcomes call, or the
// per-region outcome.record, or the FSx denial re-route, left every test passing.
// For a change whose whole purpose is "notice when the reaper is broken", being
// undetectable when broken is the one unacceptable failure mode.
// ---------------------------------------------------------------------------

// failingEC2 refuses every call with a fixed error.
type failingEC2 struct {
	err   error
	calls int
}

func (f *failingEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.calls++
	return nil, f.err
}

func (f *failingEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	return nil, f.err
}

// emptyEC2 answers successfully with no instances — a healthy scan of a fleet with
// nothing to reap, which is exactly what a totally broken run must not resemble.
type emptyEC2 struct{}

func (emptyEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func (emptyEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	return &ec2.TerminateInstancesOutput{}, nil
}

// failingFSx refuses every DescribeFileSystems call.
type failingFSx struct{ err error }

func (f *failingFSx) DescribeFileSystems(context.Context, *fsx.DescribeFileSystemsInput, ...func(*fsx.Options)) (*fsx.DescribeFileSystemsOutput, error) {
	return nil, f.err
}

func (f *failingFSx) DeleteFileSystem(context.Context, *fsx.DeleteFileSystemInput, ...func(*fsx.Options)) (*fsx.DeleteFileSystemOutput, error) {
	return nil, f.err
}

// failureReaper builds a reaper over the given accounts and two regions, with all
// optional features off so only the scan path runs.
func failureReaper(accts ...account) *reaper {
	return &reaper{
		accounts: accts,
		regions:  []string{"us-east-1", "us-west-2"},
		maxAge:   defaultMaxAge,
		dryRun:   true,
	}
}

func deniedAccount(label, id string) account {
	ec2c := &failingEC2{err: apiErr("AccessDenied")}
	return account{
		label:     label,
		accountID: id,
		ec2For:    func(string) ec2API { return ec2c },
	}
}

// The end-to-end version of plan case 1 + 4: every account refused, driven through
// handler. This is the test that dies if the feature is unplugged.
func TestHandler_AllAccountsDeniedReportsOnceAndFiresBothSentinels(t *testing.T) {
	rp := failureReaper(deniedAccount("acct-a", "111111111111"), deniedAccount("acct-b", "222222222222"))

	var sum Summary
	var err error
	out := captureLogs(func() {
		sum, err = rp.run(context.Background())
	})
	if err != nil {
		t.Fatalf("run returned %v; it must keep returning nil — an error would mark runs failed that did real work and would change EventBridge retry behaviour", err)
	}

	if sum.AccountsDenied != 2 {
		t.Errorf("accounts_denied = %d, want 2 (one per account across 2 regions each, not 4)", sum.AccountsDenied)
	}
	if sum.Errors != 0 {
		t.Errorf("errors = %d, want 0 — standing denials must not pin the operational counter (#469)", sum.Errors)
	}
	if !strings.Contains(out, sentinelAccountDenied) {
		t.Errorf("missing %q sentinel; got:\n%s", sentinelAccountDenied, out)
	}
	if !strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("missing %q sentinel — reaching zero accounts is THE signal this change exists for; got:\n%s", sentinelAllAccountsFailed, out)
	}
}

// Trap 1: a denied account must be attempted again, in every region, every run.
// The reaper's role ARN embeds a CloudFormation physical ID, so a recreated stack
// breaks every trust policy at once — dropping accounts on denial would turn a
// recoverable deploy mistake into a fleet the reaper has permanently stopped
// watching.
func TestHandler_DeniedAccountIsStillAttemptedEveryRegion(t *testing.T) {
	ec2c := &failingEC2{err: apiErr("AccessDenied")}
	rp := failureReaper(account{
		label:     "acct-a",
		accountID: "111111111111",
		ec2For:    func(string) ec2API { return ec2c },
	})

	captureLogs(func() { rp.run(context.Background()) })
	if ec2c.calls != len(rp.regions) {
		t.Errorf("DescribeInstances called %d times, want %d — quiescing means not COUNTING a denial as a surprise, never ceasing to look (#457)", ec2c.calls, len(rp.regions))
	}

	// And again next run: no state carries over to suppress it.
	captureLogs(func() { rp.run(context.Background()) })
	if ec2c.calls != 2*len(rp.regions) {
		t.Errorf("after a second run DescribeInstances called %d times, want %d — the account must never be silently dropped", ec2c.calls, 2*len(rp.regions))
	}
}

// A healthy run over an empty fleet must produce a clean, silent Summary. If this
// looked the same as the all-denied run above, the alarms would be worthless.
func TestHandler_HealthyEmptyFleetIsSilentAndDistinctFromTotalFailure(t *testing.T) {
	rp := failureReaper(account{
		label:     "acct-a",
		accountID: "111111111111",
		ec2For:    func(string) ec2API { return emptyEC2{} },
	})

	var sum Summary
	out := captureLogs(func() { sum, _ = rp.run(context.Background()) })

	if sum.Errors != 0 || sum.AccountsDenied != 0 || sum.FSxAccountsDenied != 0 {
		t.Errorf("healthy run: errors=%d denied=%d fsx_denied=%d, want 0/0/0", sum.Errors, sum.AccountsDenied, sum.FSxAccountsDenied)
	}
	for _, s := range []string{sentinelAllAccountsFailed, sentinelAccountDenied, sentinelFSxDenied} {
		if strings.Contains(out, s) {
			t.Errorf("healthy run must fire no sentinel, but logged %q:\n%s", s, out)
		}
	}
}

// Operational failure end to end: counted per region (they really are independent),
// and it still fires the correlated sentinel because zero accounts were reached.
func TestHandler_OperationalFailureCountsPerRegion(t *testing.T) {
	ec2c := &failingEC2{err: apiErr("Throttling")}
	rp := failureReaper(account{
		label:     "acct-a",
		accountID: "111111111111",
		ec2For:    func(string) ec2API { return ec2c },
	})

	var sum Summary
	out := captureLogs(func() { sum, _ = rp.run(context.Background()) })

	if sum.Errors != 2 {
		t.Errorf("errors = %d, want 2 (one per region) — operational failures are independent per region", sum.Errors)
	}
	if sum.AccountsDenied != 0 {
		t.Errorf("accounts_denied = %d, want 0 — a throttle is not a misconfigured role", sum.AccountsDenied)
	}
	if !strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("reaching zero accounts must alarm even when the cause is operational; got:\n%s", out)
	}
	if !strings.Contains(out, "operational") {
		t.Errorf("the per-region log line must carry the classification; got:\n%s", out)
	}
}

// #212 end to end: EC2 fine, FSx refused. The account-level signal stays quiet and
// the FSx one fires — the separation that would have surfaced #212.
func TestHandler_FSxDeniedWhileEC2HealthyFiresOnlyTheFSxSentinel(t *testing.T) {
	fsxc := &failingFSx{err: apiErr("AccessDenied")}
	rp := failureReaper(account{
		label:     "acct-a",
		accountID: "111111111111",
		ec2For:    func(string) ec2API { return emptyEC2{} },
		fsxFor:    func(string) fsxAPI { return fsxc },
	})

	var sum Summary
	out := captureLogs(func() { sum, _ = rp.run(context.Background()) })

	if sum.FSxAccountsDenied != 1 {
		t.Errorf("fsx_accounts_denied = %d, want 1 — once per account, not once per region", sum.FSxAccountsDenied)
	}
	if sum.Errors != 0 {
		t.Errorf("errors = %d, want 0 — an FSx denial is a standing configuration fact, counted in its own field (#469)", sum.Errors)
	}
	if sum.AccountsDenied != 0 {
		t.Errorf("accounts_denied = %d, want 0 — the account itself was reachable", sum.AccountsDenied)
	}
	if !strings.Contains(out, sentinelFSxDenied) {
		t.Errorf("missing %q; a healthy EC2 scan must not mask a missing fsx:* grant (#212); got:\n%s", sentinelFSxDenied, out)
	}
	if strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("the account WAS reached, so the correlated sentinel must stay quiet; got:\n%s", out)
	}
}

// An operational FSx failure keeps counting in Errors per region — only denials are
// re-routed. Quieting this class would be the change doing real harm.
func TestHandler_FSxOperationalFailureStillCountsInErrors(t *testing.T) {
	fsxc := &failingFSx{err: apiErr("ServiceUnavailable")}
	rp := failureReaper(account{
		label:     "acct-a",
		accountID: "111111111111",
		ec2For:    func(string) ec2API { return emptyEC2{} },
		fsxFor:    func(string) fsxAPI { return fsxc },
	})

	var sum Summary
	out := captureLogs(func() { sum, _ = rp.run(context.Background()) })

	if sum.Errors != 2 {
		t.Errorf("errors = %d, want 2 (one per region) — an FSx outage is operational and must stay loud", sum.Errors)
	}
	if sum.FSxAccountsDenied != 0 {
		t.Errorf("fsx_accounts_denied = %d, want 0 — an outage is not a missing grant", sum.FSxAccountsDenied)
	}
	if strings.Contains(out, sentinelFSxDenied) {
		t.Errorf("must not claim a missing fsx:* grant for an outage; got:\n%s", out)
	}
}

// A mixed fleet: one dead account, one healthy. The healthy one must still be
// scanned to completion, and the correlated sentinel must NOT fire — otherwise it
// stops meaning "our side is broken" and becomes noise like the old counter.
func TestHandler_MixedFleetStillScansTheHealthyAccount(t *testing.T) {
	healthy := &countingEC2{}
	rp := failureReaper(
		deniedAccount("acct-dead", "111111111111"),
		account{label: "acct-live", accountID: "222222222222", ec2For: func(string) ec2API { return healthy }},
	)

	var sum Summary
	out := captureLogs(func() { sum, _ = rp.run(context.Background()) })

	if healthy.calls != len(rp.regions) {
		t.Errorf("healthy account scanned %d times, want %d — one account's denial must never abort the run", healthy.calls, len(rp.regions))
	}
	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1", sum.AccountsDenied)
	}
	if strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("an account WAS reached, so the correlated sentinel must stay quiet; got:\n%s", out)
	}
}

// countingEC2 answers successfully and counts calls.
type countingEC2 struct{ calls int }

func (c *countingEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	c.calls++
	return &ec2.DescribeInstancesOutput{}, nil
}

func (c *countingEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	return &ec2.TerminateInstancesOutput{}, nil
}

// The registry annotation, end to end through handler: a denied account whose row
// exists gets the status printed, marked as corroboration only.
func TestHandler_DeniedAccountCarriesRegistryCorroboration(t *testing.T) {
	rp := failureReaper(deniedAccount("acct-dead", unmanagedID))
	rp.registry = &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusUnreachable)}}

	out := captureLogs(func() { rp.run(context.Background()) })

	if !strings.Contains(out, accountlifecycle.StatusUnreachable) {
		t.Errorf("the registry's verdict must be printed beside the denial; got:\n%s", out)
	}
	if !strings.Contains(out, "corroboration only") {
		t.Errorf("must mark it non-authoritative — it describes spore-portal-onboard, not the role that refused us; got:\n%s", out)
	}
}

// A denied account with no registry row must NOT be annotated with a fabricated
// status. AccountStatus() maps a missing/legacy "" to "active", so reading the map's
// zero value would print "the registry says this account is active" about an account
// the registry has never heard of — inventing a second opinion nobody holds.
func TestHandler_DeniedAccountAbsentFromRegistryIsNotFabricatedAsActive(t *testing.T) {
	rp := failureReaper(deniedAccount("acct-dead", "999999999999"))
	rp.registry = &fakeRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusDormant)}}

	out := captureLogs(func() { rp.run(context.Background()) })

	if strings.Contains(out, accountlifecycle.StatusActive) {
		t.Errorf("an absent row must never render as %q — AccountStatus() treats \"\" as active, so reading the zero value invents a verdict; got:\n%s", accountlifecycle.StatusActive, out)
	}
	if !strings.Contains(out, "no row for this account") {
		t.Errorf("absence must be stated explicitly, or the reader assumes the reassuring reading; got:\n%s", out)
	}
}

// No registry configured: no Scan, no registry commentary, and the denial still
// counts on its own.
func TestHandler_NoRegistryStillReportsTheDenial(t *testing.T) {
	rp := failureReaper(deniedAccount("acct-dead", "111111111111"))

	var sum Summary
	out := captureLogs(func() { sum, _ = rp.run(context.Background()) })

	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1 — our own AccessDenied stands without corroboration", sum.AccountsDenied)
	}
	if strings.Contains(out, "portal registry") {
		t.Errorf("with no registry configured, the log must not mention one; got:\n%s", out)
	}
}

// A healthy run must not read the registry at all. The Scan costs real
// read-capacity on a table that exists for another purpose.
func TestHandler_HealthyRunDoesNotScanTheRegistry(t *testing.T) {
	reg := &countingRegistry{rows: []accountlifecycle.Account{row(unmanagedID, accountlifecycle.StatusDormant)}}
	rp := failureReaper(account{
		label:     "acct-a",
		accountID: "111111111111",
		ec2For:    func(string) ec2API { return emptyEC2{} },
	})
	rp.registry = reg

	captureLogs(func() { rp.run(context.Background()) })

	if reg.calls != 0 {
		t.Errorf("registry scanned %d times on a healthy run, want 0 — it is only consulted to annotate a denial", reg.calls)
	}
}

// handler is the real Lambda entrypoint, and the tests above all drive run
// directly. Without this, handler could stop delegating entirely — shipping a
// reaper that returns an empty Summary and reaps nothing — and every other test
// would still pass. Mutation testing found exactly that hole.
func TestHandler_DelegatesToRun(t *testing.T) {
	prev := r
	defer func() { r = prev }()

	r = failureReaper(deniedAccount("acct-a", "111111111111"))

	var sum Summary
	out := captureLogs(func() { sum, _ = handler(context.Background()) })

	if sum.Accounts != 1 {
		t.Errorf("accounts = %d, want 1 — handler must run the configured reaper, not return an empty Summary", sum.Accounts)
	}
	if sum.AccountsDenied != 1 {
		t.Errorf("accounts_denied = %d, want 1 — the entrypoint must reach the classification path", sum.AccountsDenied)
	}
	if !strings.Contains(out, sentinelAllAccountsFailed) {
		t.Errorf("the entrypoint must emit the sentinels the alarms match; got:\n%s", out)
	}
}
